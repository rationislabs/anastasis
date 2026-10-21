package lb

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func backend(t *testing.T, id string, hits *atomic.Int64, delay time.Duration) Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		time.Sleep(delay)
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s:%s", id, body)
	}))
	t.Cleanup(srv.Close)
	return Backend{ID: id, Addr: strings.TrimPrefix(srv.URL, "http://")}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func serve(h http.Handler, method, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoundRobin(t *testing.T) {
	s := NewService("web", false)
	s.SetBackends([]Backend{backend(t, "a", nil, 0), backend(t, "b", nil, 0)})
	count := map[string]int{}
	for range 4 {
		rec := serve(s, "GET", "", nil)
		count[strings.Split(rec.Body.String(), ":")[0]]++
	}
	if count["a"] != 2 || count["b"] != 2 {
		t.Fatalf("distribution = %v", count)
	}
}

func TestNoBackends503(t *testing.T) {
	s := NewService("web", false)
	if rec := serve(s, "GET", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", rec.Code)
	}
	s.SetBackends(nil)
	if rec := serve(s, "GET", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code after SetBackends(nil) = %d", rec.Code)
	}
}

func TestSticky(t *testing.T) {
	s := NewService("web", true)
	s.SetBackends([]Backend{backend(t, "a", nil, 0), backend(t, "b", nil, 0)})
	first := serve(s, "GET", "", nil)
	cookies := first.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "anastasis_web" {
		t.Fatalf("cookies = %v", cookies)
	}
	pinned := strings.Split(first.Body.String(), ":")[0]
	for range 5 {
		if got := strings.Split(serve(s, "GET", "", cookies[0]).Body.String(), ":")[0]; got != pinned {
			t.Fatalf("sticky request went to %s, want %s", got, pinned)
		}
	}
	gone := serve(s, "GET", "", &http.Cookie{Name: "anastasis_web", Value: "gone"})
	if gone.Code != 200 || len(gone.Result().Cookies()) != 1 {
		t.Fatalf("unknown cookie: code %d, cookies %v", gone.Code, gone.Result().Cookies())
	}
}

func TestRetryOnRefused(t *testing.T) {
	var hits atomic.Int64
	s := NewService("web", false)
	s.SetBackends([]Backend{{ID: "dead", Addr: freeAddr(t)}, backend(t, "good", &hits, 0)})
	for i := range 2 {
		rec := serve(s, "POST", "hello", nil)
		if rec.Code != 200 || rec.Body.String() != "good:hello" {
			t.Fatalf("request %d: code %d body %q", i, rec.Code, rec.Body.String())
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("good backend hits = %d", hits.Load())
	}
}

func TestNoRetryOnOtherErrors(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
	defer bad.Close()
	var hits atomic.Int64
	s := NewService("web", false)
	s.SetBackends([]Backend{{ID: "bad", Addr: strings.TrimPrefix(bad.URL, "http://")}, backend(t, "good", &hits, 0)})
	codes := map[int]int{}
	for range 2 {
		codes[serve(s, "POST", "x", nil).Code]++
	}
	if codes[502] != 1 || codes[200] != 1 || hits.Load() != 1 {
		t.Fatalf("codes = %v, good hits = %d (a retry would make it 2)", codes, hits.Load())
	}
}

func TestDrainWaitsForInflight(t *testing.T) {
	s := NewService("web", false)
	s.SetBackends([]Backend{backend(t, "slow", nil, 300*time.Millisecond)})
	done := make(chan struct{})
	go func() { serve(s, "GET", "", nil); close(done) }()
	time.Sleep(50 * time.Millisecond)
	s.SetBackends(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	s.Drain(ctx, "slow")
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("Drain returned before the in-flight request finished")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request never finished")
	}
}

func TestListenSwapAndOwner(t *testing.T) {
	_, portStr, _ := net.SplitHostPort(freeAddr(t))
	var port int
	fmt.Sscan(portStr, &port)
	l := New("")
	defer l.Shutdown()
	get := func() string {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	s1 := NewService("web", false)
	s1.SetBackends([]Backend{backend(t, "a", nil, 0)})
	if err := l.Listen("p/web", port, s1); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "a:" {
		t.Fatalf("got %q", got)
	}
	s2 := NewService("web", false)
	s2.SetBackends([]Backend{backend(t, "b", nil, 0)})
	if err := l.Listen("p/web", port, s2); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "b:" {
		t.Fatalf("after swap got %q", got)
	}
	if owner, ok := l.Owner(port); !ok || owner != "p/web" {
		t.Fatalf("Owner = %q, %v", owner, ok)
	}
	l.Close("p/web")
	if _, ok := l.Owner(port); ok {
		t.Fatal("Owner after Close")
	}
}

func TestAbortedResponseReleasesInflight(t *testing.T) {
	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			if _, err := w.Write([]byte(strings.Repeat("x", 32<<10))); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer streaming.Close()
	s := NewService("web", false)
	s.SetBackends([]Backend{{ID: "stream", Addr: strings.TrimPrefix(streaming.URL, "http://")}})
	front := httptest.NewServer(s)
	defer front.Close()

	resp, err := http.Get(front.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(resp.Body, make([]byte, 1024))
	resp.Body.Close() // client goes away mid-stream
	front.CloseClientConnections()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := s.inflight["stream"]
		s.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("inflight count leaked after the client aborted; Drain would wait the full grace period")
}
func TestInflightCount(t *testing.T) {
	s := NewService("web", false)
	s.SetBackends([]Backend{backend(t, "slow", nil, 300*time.Millisecond)})
	done := make(chan struct{})
	for range 3 {
		go func() { serve(s, "GET", "", nil); done <- struct{}{} }()
	}
	time.Sleep(100 * time.Millisecond)
	if n := s.Inflight(); n != 3 {
		t.Fatalf("Inflight = %d during 3 requests", n)
	}
	for range 3 {
		<-done
	}
	if n := s.Inflight(); n != 0 {
		t.Fatalf("Inflight = %d after requests finished", n)
	}
}
func TestListenSetsIdleTimeout(t *testing.T) {
	_, portStr, _ := net.SplitHostPort(freeAddr(t))
	var port int
	fmt.Sscan(portStr, &port)
	l := New("127.0.0.1")
	defer l.Shutdown()
	if err := l.Listen("p/web", port, NewService("web", false)); err != nil {
		t.Fatal(err)
	}
	if got := l.listeners["p/web"].srv.IdleTimeout; got <= 0 {
		t.Fatalf("IdleTimeout = %v, idle keep-alive connections would stay open forever", got)
	}
}

func TestSkipEntriesExpire(t *testing.T) {
	s := NewService("web", false)
	s.markRefused("gone")
	s.mu.Lock()
	s.skip["gone"] = time.Now().Add(-time.Second) // expired
	s.mu.Unlock()
	s.markRefused("b")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.skip["gone"]; ok || len(s.skip) != 1 {
		t.Fatalf("skip = %v, want only b", s.skip)
	}
}
