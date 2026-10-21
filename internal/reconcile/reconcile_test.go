package reconcile

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/lb"
	"github.com/rationislabs/anastasis/internal/model"
	"github.com/rationislabs/anastasis/internal/plan"
	"github.com/rationislabs/anastasis/internal/runtime"
	"github.com/rationislabs/anastasis/internal/state"
)

func TestServiceWarnings(t *testing.T) {
	now := time.Now()
	got := serviceWarnings([]model.Replica{
		{Name: "a", State: "running", Health: "", Started: now.Add(-time.Hour)},
		{Name: "b", State: "running", Health: "starting", Started: now.Add(-3 * time.Minute)},
		{Name: "c", State: "running", Health: "starting", Started: now.Add(-10 * time.Second)},
	}, now)
	if len(got) != 2 || !strings.Contains(got[0], "no healthcheck") || !strings.Contains(got[1], "b:") || !strings.Contains(got[1], "enable-linger") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestStalledReplicaWarning(t *testing.T) {
	got := serviceWarnings([]model.Replica{{Name: "a", State: "running", Health: "stalled", Started: time.Now()}}, time.Now())
	if len(got) != 1 || !strings.Contains(got[0], "a: healthcheck is overdue") {
		t.Fatalf("warnings = %q", got)
	}
}

func freePort(t *testing.T) (int, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	return ln.Addr().(*net.TCPAddr).Port, ln
}

func exposed(name string, port int) model.Project {
	return model.Project{Name: name, Services: map[string]model.Service{
		"web": {Project: name, Name: "web", Expose: &model.Expose{HostPort: port, ContainerPort: 80}},
	}}
}

func TestCheckPorts(t *testing.T) {
	c := &Controller{lb: lb.New("")}
	defer c.lb.Shutdown()

	port, ln := freePort(t)
	ln.Close()
	if err := c.checkPorts(exposed("p", port)); err != nil {
		t.Fatalf("free port: %v", err)
	}
	if err := c.lb.Listen("other/web", port, lb.NewService("web", false)); err != nil {
		t.Fatal(err)
	}
	if err := c.checkPorts(exposed("p", port)); err == nil || !strings.Contains(err.Error(), "already used by other/web") {
		t.Fatalf("port owned by another service: %v", err)
	}
	if err := c.checkPorts(exposed("other", port)); err != nil {
		t.Fatalf("port owned by the same service: %v", err)
	}
	busy, foreign := freePort(t)
	defer foreign.Close()
	if err := c.checkPorts(exposed("p", busy)); err == nil {
		t.Fatal("port held by another process passed")
	}
}

func TestRolledBackIsNotConverged(t *testing.T) {
	s := ServiceStatus{Desired: 2, Ready: 2, RolledBack: "new replicas failed during rollout"}
	if s.Converged() {
		t.Fatal("a rolled-back service reported converged; up would exit 0 while the old version runs")
	}
}

func TestResetRolloutsOnUp(t *testing.T) {
	c := &Controller{mem: map[string]*memory{
		"p/web":   {rollout: plan.Rollout{Paused: true, Reason: "x"}, rolledBack: "y"},
		"other/x": {rollout: plan.Rollout{Paused: true}},
	}}
	c.resetRollouts(exposed("p", 8000))
	if m := c.mem["p/web"]; m.rollout.Paused || m.rolledBack != "" {
		t.Fatalf("p/web not reset: %+v", m)
	}
	if !c.mem["other/x"].rollout.Paused {
		t.Fatal("another project's rollout was reset")
	}
}
func TestCountReplicas(t *testing.T) {
	reps := []model.Replica{
		{ID: "old1", Hash: "a", State: "running", Health: "healthy"},
		{ID: "old2", Hash: "a", State: "running", Health: "healthy"},
		{ID: "new1", Hash: "b", State: "running", Health: "starting"},
		{ID: "new2", Hash: "b", State: "running", Health: "healthy"},
		{ID: "gone", Hash: "b", State: "running", Health: "healthy"},
	}
	var st ServiceStatus
	countReplicas(&st, reps, "b", map[string]bool{"gone": true})
	if st.Ready != 1 || st.Outdated != 2 || st.Serving != 3 {
		t.Fatalf("ready %d outdated %d serving %d, want 1 2 3", st.Ready, st.Outdated, st.Serving)
	}
}

func kinds(evs []event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestChanges(t *testing.T) {
	base := snapshot{ok: true, desired: 2, health: map[string]string{"r1": "healthy", "r2": ""}}
	until := time.Date(2026, 10, 17, 9, 0, 5, 0, time.UTC)
	for _, tc := range []struct {
		name string
		old  snapshot
		cur  func(s snapshot) snapshot
		want []string
	}{
		{"first pass records nothing", snapshot{}, func(s snapshot) snapshot { s.desired = 5; return s }, nil},
		{"nothing changed", base, func(s snapshot) snapshot { return s }, nil},
		{"scaled", base, func(s snapshot) snapshot { s.desired = 4; return s }, []string{"scaled"}},
		{"rollout started", base, func(s snapshot) snapshot { s.outdated = 2; return s }, []string{"rollout-started"}},
		{"paused", snapshot{ok: true, outdated: 2, health: map[string]string{}}, func(s snapshot) snapshot { s.paused, s.reason = true, "new replicas failed"; return s }, []string{"paused"}},
		{"rolled back", base, func(s snapshot) snapshot { s.rolledBack = "rolled back"; return s }, []string{"rolled-back"}},
		{"new error", base, func(s snapshot) snapshot { s.err = "pull x: denied"; return s }, []string{"error"}},
		{"crash backoff", base, func(s snapshot) snapshot { s.backoffUntil = until; return s }, []string{"crash-backoff"}},
		{"health change", base, func(s snapshot) snapshot {
			s.health = map[string]string{"r1": "stalled", "r2": "", "r3": "starting"} // r3 is new: no event
			return s
		}, []string{"health"}},
	} {
		old := tc.old
		cur := tc.cur(snapshot{ok: true, desired: old.desired, outdated: old.outdated, paused: old.paused, health: copyHealth(old.health)})
		if got := kinds(changes(old, cur, "p", "web")); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: kinds = %v, want %v", tc.name, got, tc.want)
		}
	}
	done := changes(snapshot{ok: true, outdated: 1, health: map[string]string{}}, snapshot{ok: true, health: map[string]string{}}, "p", "web")
	if len(done) != 1 || done[0].Kind != "rollout-done" || done[0].Project != "p" || done[0].Service != "web" {
		t.Fatalf("rollout done = %+v", done)
	}
	h := changes(base, snapshot{ok: true, desired: 2, health: map[string]string{"r1": "stalled", "r2": ""}}, "p", "web")
	if h[0].Replica != "r1" || h[0].Text != "healthy → stalled" {
		t.Fatalf("health event = %+v", h[0])
	}
}

func copyHealth(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// engine is a fake Podman with no containers that says yes to everything
// else: images and networks exist, creates and removes succeed.
func engine(t *testing.T) *runtime.Runtime {
	t.Helper()
	dir, err := os.MkdirTemp("", "rc") // short path: unix socket names max out near 108 bytes
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
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			io.WriteString(w, "[]")
			return
		}
		io.WriteString(w, "{}")
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	rt, _ := runtime.New(sock)
	return rt
}

func web(replicas int) (state.ProjectState, model.Service) {
	svc := model.Service{Project: "p", Name: "web", Hash: "h", Replicas: replicas, Config: model.Config{Image: "x"},
		Update: model.UpdateConfig{Parallelism: 1, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "pause"}}
	return state.ProjectState{Current: model.Project{Name: "p", Services: map[string]model.Service{"web": svc}}}, svc
}

// A service removed from the file while it has no containers still loses
// its listener, its memory and its autoscaler state.
func TestPassForgetsRemovedServiceWithoutContainers(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Up(model.Project{Name: "p", Services: map[string]model.Service{}}, "/p/compose.json"); err != nil {
		t.Fatal(err)
	}
	c := New(store, engine(t), lb.New(""))
	defer c.lb.Shutdown()
	port, ln := freePort(t)
	ln.Close()
	svc := lb.NewService("old", false)
	if err := c.lb.Listen("p/old", port, svc); err != nil {
		t.Fatal(err)
	}
	c.mem["p/old"] = &memory{lb: svc}
	c.auto.Track("p/old", model.Autoscale{Min: 1, Max: 3}, svc, nil, 2, false, false)

	c.pass(context.Background())
	if owner, ok := c.lb.Owner(port); ok {
		t.Fatalf("port %d is still held by %s", port, owner)
	}
	if c.mem["p/old"] != nil {
		t.Fatal("memory kept")
	}
	if _, _, _, ok := c.auto.Desired("p/old"); ok {
		t.Fatal("autoscaler state kept")
	}
}

func TestEventsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	c := New(nil, engine(t), lb.New(""))
	ps, svc := web(1)
	c.reconcileService(context.Background(), ps, svc, nil)
	if !strings.Contains(buf.String(), "created") {
		t.Fatalf("no log line for the created replica; log:\n%s", buf.String())
	}
}

func TestBackoffEndPokes(t *testing.T) {
	c := New(nil, engine(t), lb.New(""))
	ps, svc := web(1)
	now := time.Now()
	crashed := model.Replica{ID: "a", Name: "p-web-a", Service: "web", Hash: "h", State: "exited", Started: now.Add(-3 * time.Second), Finished: now.Add(-time.Second)}
	c.reconcileService(context.Background(), ps, svc, []model.Replica{crashed})
	until := c.mem["p/web"].backoff.Until
	if !until.After(now) {
		t.Fatal("no crash backoff; the test needs one")
	}
	select {
	case <-c.poke:
		if time.Now().Before(until) {
			t.Fatal("poked before the backoff ended")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the backoff ended without waking the loop")
	}
}

// After a restart, a replica that died while anastasis was down still counts.
func TestAutoscaleInitialCountsDeadReplicas(t *testing.T) {
	c := New(nil, nil, nil)
	svc := model.Service{Replicas: 1, Autoscale: &model.Autoscale{Min: 1, Max: 5}}
	reps := []model.Replica{{ID: "a", Hash: "h", State: "running"}, {ID: "b", Hash: "h", State: "running"}, {ID: "c", Hash: "h", State: "exited"}}
	if n, _, _ := c.autoscaled("p/web", svc, &memory{}, reps, "h"); n != 3 {
		t.Fatalf("initial desired = %d, want 3", n)
	}
}
