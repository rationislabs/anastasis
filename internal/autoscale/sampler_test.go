package autoscale

import (
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/lb"
	"github.com/rationislabs/anastasis/internal/model"
	"github.com/rationislabs/anastasis/internal/runtime"
)

func TestCPUPercent(t *testing.T) {
	prev := cpuSample{usage: time.Second, at: now}
	cur := cpuSample{usage: 1500 * time.Millisecond, at: now.Add(time.Second)}
	if got, ok := cpuPercent(prev, cur, 0.5); !ok || math.Abs(got-100) > 0.001 {
		t.Fatalf("cpuPercent = %v, %v; want 100 (0.5 cores used of a 0.5 limit)", got, ok)
	}
}

func TestTrackClampsAndStartsFromInitial(t *testing.T) {
	s := NewSampler(nil, DefaultWindows)
	s.Track("p/web", model.Autoscale{Min: 1, Max: 4, Inflight: 5}, nil, nil, 6, false, false)
	if n, reason, _, ok := s.Desired("p/web"); !ok || n != 4 || reason != "waiting for data" {
		t.Fatalf("Desired = %d, %q, %v; want initial 6 clamped to max 4, waiting for data", n, reason, ok)
	}
	s.Track("p/web", model.Autoscale{Min: 1, Max: 2, Inflight: 5}, nil, nil, 6, false, false)
	if n, _, _, _ := s.Desired("p/web"); n != 2 {
		t.Fatalf("Desired = %d after max lowered to 2", n)
	}
	s.Forget("p/web")
	if _, _, _, ok := s.Desired("p/web"); ok {
		t.Fatal("Desired after Forget")
	}
}

// fakeRuntime serves the Podman API on a unix socket; stats answers every
// request but /version.
func fakeRuntime(t *testing.T, stats http.HandlerFunc) *runtime.Runtime {
	t.Helper()
	dir, err := os.MkdirTemp("", "as") // short path: unix socket names max out near 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			io.WriteString(w, `{"ApiVersion":"1.41","MinAPIVersion":"1.24"}`)
			return
		}
		stats(w, r)
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	rt, _ := runtime.New(sock)
	return rt
}

// answers serves bodies in order, repeating the last one.
func answers(bodies ...string) http.HandlerFunc {
	var n atomic.Int32
	return func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, bodies[min(int(n.Add(1)), len(bodies))-1])
	}
}

var cpuJobA = cpuJob{ids: []string{"a"}, limit: 0.5}

// Wall time comes from the runtime's read timestamps, not from when the
// stats call returned.
func TestCPUUsesRuntimeTimestamps(t *testing.T) {
	s := NewSampler(fakeRuntime(t, answers(
		`{"read":"2026-10-09T12:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":1000000000}}}`,
		`{"read":"2026-10-09T12:00:01Z","cpu_stats":{"cpu_usage":{"total_usage":1500000000}}}`,
	)), DefaultWindows)
	s.measureCPU(context.Background(), "p/web", cpuJobA)
	if pct, reason := s.measureCPU(context.Background(), "p/web", cpuJobA); math.Abs(pct-100) > 0.001 {
		t.Fatalf("cpu = %v (%q); want 100: 0.5 s of CPU over 1 s of runtime time, 0.5 limit", pct, reason)
	}
}

// An in-place restart resets the cumulative counter; that sample is a first
// sample, never a negative percent.
func TestCounterResetIsFirstSample(t *testing.T) {
	s := NewSampler(fakeRuntime(t, answers(
		`{"read":"2026-10-09T12:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":5000000000}}}`,
		`{"read":"2026-10-09T12:00:01Z","cpu_stats":{"cpu_usage":{"total_usage":100000000}}}`,
		`{"read":"2026-10-09T12:00:02Z","cpu_stats":{"cpu_usage":{"total_usage":350000000}}}`,
	)), DefaultWindows)
	s.measureCPU(context.Background(), "p/web", cpuJobA)
	if pct, reason := s.measureCPU(context.Background(), "p/web", cpuJobA); pct != -1 || reason != "waiting for a second sample" {
		t.Fatalf("after a counter reset: cpu = %v, %q; want -1, waiting for a second sample", pct, reason)
	}
	if pct, _ := s.measureCPU(context.Background(), "p/web", cpuJobA); math.Abs(pct-50) > 0.001 {
		t.Fatalf("cpu = %v after the reset; want 50 measured from the new counter", pct)
	}
}

// A hung stats call must not stop the in-flight ring from filling.
func TestHungStatsDoNotStallInflight(t *testing.T) {
	release := make(chan struct{})
	rt := fakeRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) }) // runs before the server closes
	s := NewSampler(rt, Windows{Check: time.Millisecond, Average: time.Minute, Down: time.Minute, Step: time.Minute})
	s.Track("p/web", model.Autoscale{Min: 1, Max: 4, CPU: 70, CPULimit: 0.5}, lb.NewService("web", false), []string{"a"}, 1, false, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, func() {})
	for deadline := time.Now().Add(2*sampleEvery + time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		s.mu.Lock()
		n := len(s.svcs["p/web"].inflight)
		s.mu.Unlock()
		if n > 0 {
			return
		}
	}
	t.Fatal("no in-flight sample while a stats call hung")
}

// A changed desired count wakes the reconcile loop instead of waiting for
// its next pass.
func TestRunCallsChangedOnNewCount(t *testing.T) {
	s := NewSampler(nil, Windows{Check: time.Millisecond, Average: time.Minute, Down: time.Hour, Step: time.Hour})
	s.Track("p/web", model.Autoscale{Min: 1, Max: 4, Inflight: 5}, nil, []string{"a"}, 1, false, false)
	s.svcs["p/web"].inflight = []float64{20}
	changed := make(chan struct{}, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, func() { changed <- struct{}{} })
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("changed not called after the desired count went 1→4")
	}
	time.Sleep(50 * time.Millisecond) // many more checks, same count
	if len(changed) != 0 {
		t.Fatalf("changed called %d more times with the count unchanged", len(changed))
	}
}
