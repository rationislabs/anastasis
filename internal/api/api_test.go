package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/reconcile"
)

// A hung serve must not hang the CLI: reads time out.
func TestGetTimesOut(t *testing.T) {
	old := getTimeout
	getTimeout = 100 * time.Millisecond
	defer func() { getTimeout = old }()
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	defer close(release)
	done := make(chan error, 1)
	go func() { _, err := NewClient(sock).Status(""); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("no error from a hung serve")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Status still waiting on a hung serve")
	}
}

func TestUpNeedsAbsolutePath(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ln, reconcile.New(nil, nil, nil))
	if _, err := NewClient(sock).Up("compose.json", nil); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Up with a relative path: %v", err)
	}
}

func TestListenRefusesLiveSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock) // another serve, started with a different -state
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got, err := Listen(sock); err == nil || !strings.Contains(err.Error(), "already") {
		if got != nil {
			got.Close()
		}
		t.Fatalf("Listen = %v, want refusal: it took over a socket another process answers on", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false) // a crash leaves the file behind
	ln.Close()
	if ln, err = Listen(sock); err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, reconcile.New(nil, nil, nil)) }()
	cl := NewClient(sock)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, err := cl.Status("")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale socket not replaced: %v", err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// One huge request must not run anastasis out of memory.
func TestRequestBodyLimited(t *testing.T) {
	h := handler(reconcile.New(nil, nil, nil))
	body := strings.NewReader(`{"path":"/` + strings.Repeat("x", maxBody) + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/up", body))
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("oversized request: %d %.200s", rec.Code, rec.Body.String())
	}
}
