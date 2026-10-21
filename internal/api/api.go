// Package api serves the reconcile controller as JSON over HTTP on a unix
// socket for the CLI (the socket's file mode, 0600, is the authentication).
// It is also the client the CLI uses.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rationislabs/anastasis/internal/reconcile"
)

type upRequest struct {
	Path string   `json:"path"`
	Env  []string `json:"env"`
}

type upResponse struct {
	Project string `json:"project"`
}

type downRequest struct {
	Project string `json:"project"`
	Volumes bool   `json:"volumes"`
}

type scaleRequest struct {
	Project string         `json:"project"`
	Scale   map[string]int `json:"scale"`
}

type restartRequest struct {
	Project string `json:"project"`
	Service string `json:"service"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Listen opens the CLI socket. Call it before starting anything else, so
// a second serve stops before it touches containers.
func Listen(socket string) (net.Listener, error) {
	// The state lock does not cover the socket: a serve with another -state
	// uses the same one. Only a socket nobody answers on is a stale one.
	// ponytail: two serves starting at the same instant can still both pass
	// this check; lock the socket path too if that ever happens.
	if conn, err := net.Dial("unix", socket); err == nil {
		conn.Close()
		return nil, fmt.Errorf("another anastasis serve already answers on %s; stop it or pass -socket", socket)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	old := syscall.Umask(0o177) // create the socket as 0600, with no window where others can connect
	ln, err := net.Listen("unix", socket)
	syscall.Umask(old)
	return ln, err
}

// Serve serves the CLI on ln, from Listen, until ctx ends.
func Serve(ctx context.Context, ln net.Listener, c *reconcile.Controller) error {
	srv := &http.Server{Handler: handler(c)}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func handler(c *reconcile.Controller) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /up", func(w http.ResponseWriter, r *http.Request) {
		var req upRequest
		if !decode(w, r, &req) {
			return
		}
		if !filepath.IsAbs(req.Path) {
			reply(w, nil, errors.New("path must be absolute"))
			return
		}
		name, err := c.Up(r.Context(), req.Path, req.Env)
		reply(w, upResponse{name}, err)
	})
	mux.HandleFunc("POST /down", func(w http.ResponseWriter, r *http.Request) {
		var req downRequest
		if decode(w, r, &req) {
			reply(w, struct{}{}, c.Down(r.Context(), req.Project, req.Volumes))
		}
	})
	mux.HandleFunc("POST /scale", func(w http.ResponseWriter, r *http.Request) {
		var req scaleRequest
		if decode(w, r, &req) {
			reply(w, struct{}{}, c.Scale(r.Context(), req.Project, req.Scale))
		}
	})
	mux.HandleFunc("POST /rollout-restart", func(w http.ResponseWriter, r *http.Request) {
		var req restartRequest
		if decode(w, r, &req) {
			reply(w, struct{}{}, c.RolloutRestart(r.Context(), req.Project, req.Service))
		}
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		reply(w, c.Status(r.URL.Query().Get("project")), nil)
	})
	return mux
}

// maxBody caps a request body.
const maxBody = 16 << 20

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			err = fmt.Errorf("request too large (over %d MiB)", maxBody>>20)
		}
		reply(w, nil, fmt.Errorf("bad request: %w", err))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{err.Error()})
		return
	}
	json.NewEncoder(w).Encode(v)
}

// Client talks to a running anastasis serve.
type Client struct {
	hc   *http.Client
	base string
}

// NewClient returns a client for the unix socket at path.
func NewClient(socket string) *Client {
	return &Client{base: "http://anastasis", hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *Client) Up(path string, env []string) (string, error) {
	var out upResponse
	err := c.call("POST", "/up", upRequest{path, env}, &out)
	return out.Project, err
}

func (c *Client) Down(project string, volumes bool) error {
	return c.call("POST", "/down", downRequest{project, volumes}, nil)
}

func (c *Client) Scale(project string, scale map[string]int) error {
	return c.call("POST", "/scale", scaleRequest{project, scale}, nil)
}

func (c *Client) RolloutRestart(project, service string) error {
	return c.call("POST", "/rollout-restart", restartRequest{project, service}, nil)
}

func (c *Client) Status(project string) (reconcile.Status, error) {
	var st reconcile.Status
	err := c.call("GET", "/status?project="+url.QueryEscape(project), nil, &st)
	return st, err
}

// getTimeout bounds the CLI's reads, so a hung serve is
// reported instead of waited on forever. Writes (up, down) may take minutes.
var getTimeout = 10 * time.Second

func (c *Client) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx := context.Background()
	if method == "GET" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, getTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach anastasis serve (is it running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errorFrom(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// errorFrom turns a failed answer into an error.
func errorFrom(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e errorResponse
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return errors.New(e.Error)
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return errors.New(s)
	}
	return fmt.Errorf("anastasis serve answered %s", resp.Status)
}
