// Package lb is anastasis's HTTP load balancer: one listener per exposed
// service, round-robin over ready replicas, optional sticky sessions.
package lb

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const skipRefused = 5 * time.Second

// Backend is one replica the load balancer can send requests to.
type Backend struct {
	ID   string // container ID
	Addr string // "127.0.0.1:<port>"
}

// Service balances requests across one service's replicas.
type Service struct {
	name   string
	sticky bool
	proxy  *httputil.ReverseProxy
	next   atomic.Uint64

	// mu guards backends and inflight together, so after SetBackends returns
	// no new request can pick a removed backend and Drain sees every request.
	mu       sync.Mutex
	backends []Backend
	inflight map[string]int
	skip     map[string]time.Time
}

type ctxKey struct{}

type attempt struct {
	backend Backend
	retry   bool // a refused dial may be retried on another backend
	refused bool // set by the error handler
}

// NewService returns a balancer for one service.
func NewService(name string, sticky bool) *Service {
	s := &Service{name: name, sticky: sticky, inflight: map[string]int{}, skip: map[string]time.Time{}}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			a := pr.In.Context().Value(ctxKey{}).(*attempt)
			pr.SetURL(&url.URL{Scheme: "http", Host: a.backend.Addr})
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			if !s.sticky {
				return nil
			}
			a := resp.Request.Context().Value(ctxKey{}).(*attempt)
			if c, err := resp.Request.Cookie(s.cookie()); err != nil || c.Value != a.backend.ID {
				resp.Header.Add("Set-Cookie", (&http.Cookie{Name: s.cookie(), Value: a.backend.ID, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			a := r.Context().Value(ctxKey{}).(*attempt)
			if errors.Is(err, syscall.ECONNREFUSED) {
				s.markRefused(a.backend.ID)
				if a.retry {
					a.refused = true // nothing was sent; ServeHTTP retries
					return
				}
			}
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	return s
}

// Sticky reports whether the service pins clients to a replica.
func (s *Service) Sticky() bool { return s.sticky }

func (s *Service) cookie() string { return "anastasis_" + s.name }

// SetBackends replaces the replicas that receive new requests.
func (s *Service) SetBackends(bs []Backend) {
	s.mu.Lock()
	s.backends = slices.Clone(bs)
	s.mu.Unlock()
}

// Drain waits until no request is in flight to backend id, or ctx ends.
// Call it after removing id with SetBackends.
func (s *Service) Drain(ctx context.Context, id string) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		s.mu.Lock()
		n := s.inflight[id]
		s.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Inflight returns the number of requests in flight across all replicas.
func (s *Service) Inflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.inflight {
		n += v
	}
	return n
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A refused dial sends no bytes, so the unread body can go to the retry.
	r.Body = io.NopCloser(r.Body)
	tried := ""
	for i := range 2 {
		b, ok := s.pick(r, tried)
		if !ok {
			if i == 0 {
				http.Error(w, "no ready replicas", http.StatusServiceUnavailable)
			} else {
				http.Error(w, "bad gateway", http.StatusBadGateway)
			}
			return
		}
		a := &attempt{backend: b, retry: i == 0}
		s.forward(w, r, a)
		if !a.refused {
			return
		}
		tried = b.ID
	}
}

// forward proxies one attempt. The release is deferred because ReverseProxy
// panics with http.ErrAbortHandler when the client goes away mid-response.
func (s *Service) forward(w http.ResponseWriter, r *http.Request, a *attempt) {
	defer s.release(a.backend.ID)
	s.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, a)))
}

// pick chooses a backend and counts the request as in flight to it.
func (s *Service) pick(r *http.Request, exclude string) (Backend, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.choose(r, exclude)
	if ok {
		s.inflight[b.ID]++
	}
	return b, ok
}

func (s *Service) choose(r *http.Request, exclude string) (Backend, bool) {
	if s.sticky {
		if c, err := r.Cookie(s.cookie()); err == nil && c.Value != exclude {
			for _, b := range s.backends {
				if b.ID == c.Value {
					return b, true
				}
			}
		}
	}
	now := time.Now()
	var cands []Backend
	for _, b := range s.backends {
		if b.ID != exclude && now.After(s.skip[b.ID]) {
			cands = append(cands, b)
		}
	}
	if len(cands) == 0 { // every backend refused recently: try them anyway
		for _, b := range s.backends {
			if b.ID != exclude {
				cands = append(cands, b)
			}
		}
	}
	if len(cands) == 0 {
		return Backend{}, false
	}
	return cands[s.next.Add(1)%uint64(len(cands))], true
}

func (s *Service) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id]--; s.inflight[id] <= 0 {
		delete(s.inflight, id)
	}
}

func (s *Service) markRefused(id string) {
	now := time.Now()
	s.mu.Lock()
	maps.DeleteFunc(s.skip, func(_ string, until time.Time) bool { return now.After(until) })
	s.skip[id] = now.Add(skipRefused)
	s.mu.Unlock()
}

// LB owns one listener per exposed service.
type LB struct {
	addr      string // "" listens on all interfaces
	mu        sync.Mutex
	listeners map[string]*listener // key: "project/service" or "fwd:project/service"
}

type listener struct {
	port int
	srv  *http.Server
	h    atomic.Pointer[handlerBox]
}

type handlerBox struct{ http.Handler }

// readTimeout is how long a request body may stall: a client that stops
// sending would otherwise hold a connection, and count as in flight, for as
// long as it likes. It bounds each wait for the next bytes, not the whole
// body, so a large upload on a slow line still arrives.
var readTimeout = 30 * time.Second

// New returns an LB that listens on addr ("" for all interfaces).
func New(addr string) *LB { return &LB{addr: addr, listeners: map[string]*listener{}} }

// Listen serves h on port under key. With the same key and port it only
// swaps the handler; a new port replaces the listener.
func (l *LB) Listen(key string, port int, h http.Handler) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ln, ok := l.listeners[key]; ok {
		if ln.port == port {
			ln.h.Store(&handlerBox{h})
			return nil
		}
		ln.srv.Close()
		delete(l.listeners, key)
	}
	nl, err := net.Listen("tcp", net.JoinHostPort(l.addr, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	ln := &listener{port: port}
	ln.h.Store(&handlerBox{h})
	ln.srv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			rc.SetReadDeadline(time.Now().Add(readTimeout))
			r.Body = &idleBody{r.Body, rc}
			ln.h.Load().ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go ln.srv.Serve(nl)
	l.listeners[key] = ln
	return nil
}

// Close stops the listener for key, if any.
func (l *LB) Close(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ln, ok := l.listeners[key]; ok {
		ln.srv.Close()
		delete(l.listeners, key)
	}
}

// Owner returns the key listening on port.
func (l *LB) Owner(port int) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, ln := range l.listeners {
		if ln.port == port {
			return key, true
		}
	}
	return "", false
}

// Shutdown stops every listener.
func (l *LB) Shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, ln := range l.listeners {
		ln.srv.Close()
		delete(l.listeners, key)
	}
}

// idleBody moves the read deadline forward before every read of a request
// body: the deadline catches a stalled client, not a slow but steady one.
type idleBody struct {
	io.ReadCloser
	rc *http.ResponseController
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.rc.SetReadDeadline(time.Now().Add(readTimeout))
	return b.ReadCloser.Read(p)
}
