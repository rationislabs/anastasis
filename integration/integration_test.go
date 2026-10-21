//go:build integration

// Integration tests run anastasis against a real Podman or Docker API.
// Locally: ANASTASIS_TEST_RUNTIME=/var/run/docker.sock ANASTASIS_TEST_CLI=docker go test -tags integration -count=1 -timeout 15m ./integration/
package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/lb"
	"github.com/rationislabs/anastasis/internal/reconcile"
	"github.com/rationislabs/anastasis/internal/runtime"
	"github.com/rationislabs/anastasis/internal/state"
)

const (
	project     = "anastasisit"
	composeTmpl = `name: anastasisit
services:
  web:
    image: docker.io/library/nginx:1.27-alpine
    environment:
      VERSION: "%s"
    healthcheck:
      test: %s
      interval: 1s
      timeout: 2s
      retries: 2
    deploy:
      replicas: 3
      update_config: { parallelism: 1, order: start-first, monitor: 60s }
    x-anastasis:
      expose: "18080:80"
`
	healthy = `["CMD", "wget", "-qO-", "http://127.0.0.1/"]`
	broken  = `["CMD", "false"]`
)

func runtimeSocket() string {
	if s := os.Getenv("ANASTASIS_TEST_RUNTIME"); s != "" {
		return s
	}
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "podman", "podman.sock")
}

func cli() string {
	if c := os.Getenv("ANASTASIS_TEST_CLI"); c != "" {
		return c
	}
	return "podman"
}

type harness struct {
	t      *testing.T
	ctrl   *reconcile.Controller
	rt     *runtime.Runtime
	store  *state.Store
	lb     *lb.LB
	cancel context.CancelFunc
	done   chan struct{}
	// One Compose file path for every version: anastasis refuses a project name
	// registered from a different file.
	compose string
}

func start(t *testing.T, statePath string) *harness { return startWith(t, statePath, nil) }

func startWith(t *testing.T, statePath string, configure func(*reconcile.Controller)) *harness {
	t.Helper()
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.New(runtimeSocket())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, rt: rt, store: store, lb: lb.New(""), done: make(chan struct{}), compose: filepath.Join(filepath.Dir(statePath), "compose.yaml")}
	h.ctrl = reconcile.New(store, rt, h.lb)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	if configure != nil {
		configure(h.ctrl)
	}
	go func() { h.ctrl.Run(ctx); close(h.done) }()
	return h
}

func (h *harness) stop() {
	h.cancel()
	<-h.done
	h.lb.Shutdown()
	h.store.Close()
}

func (h *harness) up(version, health string) {
	h.t.Helper()
	path := h.compose
	if err := os.WriteFile(path, []byte(fmt.Sprintf(composeTmpl, version, health)), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.ctrl.Up(context.Background(), path, nil); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) web() reconcile.ServiceStatus {
	for _, s := range h.ctrl.Status(project).Services {
		if s.Service == "web" {
			return s
		}
	}
	return reconcile.ServiceStatus{}
}

func (h *harness) waitFor(what string, timeout time.Duration, ok func(reconcile.ServiceStatus) bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok(h.web()) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s; last status %+v", what, h.web())
}

func (h *harness) replicaIDs() []string {
	h.t.Helper()
	all, err := h.rt.List(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var ids []string
	for _, r := range all[project] {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

func (h *harness) removeLeftovers() {
	all, err := h.rt.List(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, r := range all[project] {
		h.rt.Remove(context.Background(), r.ID, 0)
	}
}

func converged(n int) func(reconcile.ServiceStatus) bool {
	return func(s reconcile.ServiceStatus) bool { return s.Converged() && s.Ready == n }
}

func get(port int) (int, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

func getURL(url string) (int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

func allOK(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		if code, err := get(18080); err != nil || code != 200 {
			t.Fatalf("request %d: code %d, err %v", i, code, err)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.json")
	h := start(t, statePath)
	h.removeLeftovers()
	t.Cleanup(func() {
		h.ctrl.Down(ctx, project, true)
		h.stop()
	})

	h.up("1", healthy)
	h.waitFor("3 ready replicas", 3*time.Minute, converged(3))
	allOK(t, 10)

	t.Log("self-healing")
	victim := h.replicaIDs()[0]
	if out, err := exec.Command(cli(), "kill", victim).CombinedOutput(); err != nil {
		t.Fatalf("kill: %v\n%s", err, out)
	}
	h.waitFor("killed replica replaced", time.Minute, func(s reconcile.ServiceStatus) bool {
		return converged(3)(s) && !slices.Contains(h.replicaIDs(), victim)
	})

	t.Log("scaling")
	for _, n := range []int{5, 1, 3} {
		if err := h.ctrl.Scale(ctx, project, map[string]int{"web": n}); err != nil {
			t.Fatal(err)
		}
		h.waitFor(fmt.Sprintf("%d replicas", n), 2*time.Minute, func(s reconcile.ServiceStatus) bool {
			return converged(n)(s) && len(h.replicaIDs()) == n
		})
		allOK(t, 10)
	}

	t.Log("zero-downtime rollout")
	var reqs, failures atomic.Int64
	stop, loopDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(loopDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			reqs.Add(1)
			if code, err := get(18080); err != nil || code != 200 {
				failures.Add(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	h.up("2", healthy)
	h.waitFor("rollout to version 2", 3*time.Minute, converged(3))
	close(stop)
	<-loopDone
	if failures.Load() != 0 || reqs.Load() < 50 {
		t.Fatalf("%d of %d requests failed during the rollout", failures.Load(), reqs.Load())
	}

	t.Log("failed rollout pauses")
	h.up("3", broken)
	h.waitFor("paused rollout", 2*time.Minute, func(s reconcile.ServiceStatus) bool { return s.Paused })
	allOK(t, 20)
	h.up("4", healthy)
	h.waitFor("recovered rollout", 3*time.Minute, converged(3))

	t.Log("adoption after controller restart")
	before := h.replicaIDs()
	h.stop()
	h = start(t, statePath)
	h.waitFor("adopted replicas", time.Minute, converged(3))
	if after := h.replicaIDs(); !slices.Equal(before, after) {
		t.Fatalf("replicas recreated on restart: before %v after %v", before, after)
	}
	allOK(t, 10)
}

const cliCompose = `name: anastasiscli
services:
  web:
    image: docker.io/library/nginx:1.27-alpine
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1/"]
      interval: 1s
      retries: 2
    deploy:
      replicas: 2
    x-anastasis:
      expose: "18081:80"
`

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "anastasis")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/anastasis").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sock := filepath.Join(dir, "anastasis.sock")
	serve := exec.Command(bin, "-socket", sock, "serve", "-state", filepath.Join(dir, "state.json"), "-runtime", runtimeSocket())
	serve.Stderr = os.Stderr
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		serve.Process.Signal(os.Interrupt)
		serve.Wait()
	})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	anastasis := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, append([]string{"-socket", sock}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("anastasis %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	compose := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(compose, []byte(cliCompose), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := anastasis("up", "-f", compose); !strings.Contains(out, "2/2") {
		t.Fatalf("up output:\n%s", out)
	}
	anastasis("scale", "anastasiscli", "web=1")
	deadline := time.Now().Add(time.Minute)
	for !strings.Contains(anastasis("status", "anastasiscli"), "1/1") {
		if time.Now().After(deadline) {
			t.Fatalf("scale never reached 1/1:\n%s", anastasis("status", "anastasiscli"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	anastasis("down", "anastasiscli", "--volumes")
	if out := anastasis("status"); strings.Contains(out, "anastasiscli") {
		t.Fatalf("project still listed after down:\n%s", out)
	}
}
