package runtime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/config"
	"github.com/rationislabs/anastasis/internal/model"
	"github.com/rationislabs/anastasis/internal/podman"
)

func loadService(t *testing.T, yaml string) model.Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := config.Load(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p.Services["web"]
}

func TestEnsureBindSources(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a", "b")
	svc := loadService(t, "name: t\nservices:\n  web:\n    image: nginx\n    volumes: [\""+src+":/data\"]\n")
	if err := ensureBindSources(svc); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		t.Fatalf("bind source not created: %v", err)
	}
}
func TestContainerSpec(t *testing.T) {
	path, _ := filepath.Abs("testdata/spec.yaml")
	p, err := config.Load(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := containerSpec(p.Services["web"], "h1")

	if s.Image != "docker.io/library/nginx:1.27-alpine" || !slices.Contains(s.Env, "A=1") {
		t.Fatalf("image %q env %v", s.Image, s.Env)
	}
	wantLabels := map[string]string{"team": "home", LabelProject: "spec", LabelService: "web", LabelHash: "h1"}
	if !reflect.DeepEqual(s.Labels, wantLabels) {
		t.Fatalf("labels = %v (anastasis labels must win over user labels)", s.Labels)
	}
	if hc := s.Healthcheck; hc == nil || hc.Interval != 5*time.Second || hc.Retries != 3 || hc.Test[0] != "CMD" {
		t.Fatalf("healthcheck = %+v", s.Healthcheck)
	}
	h := s.HostConfig
	if got := h.PortBindings["80/tcp"]; !reflect.DeepEqual(got, []podman.PortBinding{{HostIP: "127.0.0.1"}}) {
		t.Fatalf("port bindings = %v", h.PortBindings)
	}
	if _, ok := s.ExposedPorts["80/tcp"]; !ok {
		t.Fatalf("exposed ports = %v", s.ExposedPorts)
	}
	if !reflect.DeepEqual(h.Mounts, []podman.Mount{{Type: "volume", Source: "spec_data", Target: "/data"}}) {
		t.Fatalf("mounts = %+v", h.Mounts)
	}
	if h.NanoCPUs != 500_000_000 || h.Memory != 256<<20 {
		t.Fatalf("limits cpu=%d mem=%d", h.NanoCPUs, h.Memory)
	}
	if !reflect.DeepEqual(h.CapDrop, []string{"ALL"}) || !reflect.DeepEqual(s.Cmd, []string{"nginx", "-g", "daemon off;"}) {
		t.Fatalf("capdrop %v cmd %v", h.CapDrop, s.Cmd)
	}
	if ep, ok := s.NetworkingConfig.EndpointsConfig["anastasis-spec"]; !ok || !reflect.DeepEqual(ep.Aliases, []string{"web"}) || h.NetworkMode != "anastasis-spec" {
		t.Fatalf("network = %+v mode %q", s.NetworkingConfig, h.NetworkMode)
	}
}

func TestHealthcheckDisableOverridesImage(t *testing.T) {
	svc := loadService(t, "name: t\nservices:\n  web:\n    image: nginx\n    healthcheck: {disable: true}\n")
	if s := containerSpec(svc, "h"); s.Healthcheck == nil || !reflect.DeepEqual(s.Healthcheck.Test, []string{"NONE"}) {
		t.Fatalf("healthcheck = %+v, want Test [NONE] so the image's HEALTHCHECK is off too", s.Healthcheck)
	}
}

func TestSELinuxBindKeepsLabel(t *testing.T) {
	src := t.TempDir()
	svc := loadService(t, "name: t\nservices:\n  web:\n    image: nginx\n    volumes: [\""+src+":/data:ro,z\"]\n")
	h := containerSpec(svc, "h").HostConfig
	if len(h.Mounts) != 0 || !reflect.DeepEqual(h.Binds, []string{src + ":/data:ro,z"}) {
		t.Fatalf("mounts %+v binds %v, want the bind with its :z label", h.Mounts, h.Binds)
	}
}

// Podman 4.9 never times out a hung healthcheck: the status stays at its
// last value. A check overdue past interval+timeout takes the replica out
// of the load balancer; overdue past retries of those, it is replaced.
func TestStalledHealthcheck(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	inspect := func(status string, hc *podman.Healthcheck, lastCheck time.Duration) podman.Container {
		var c podman.Container
		c.Config.Healthcheck = hc
		c.State.StartedAt = now.Add(-time.Hour).Format(time.RFC3339Nano)
		if status != "" {
			c.State.Health = &podman.Health{Status: status}
			if lastCheck > 0 {
				c.State.Health.Log = []podman.HealthLog{
					{Start: now.Add(-lastCheck - 3*time.Second).Format(time.RFC3339Nano)},
					{Start: now.Add(-lastCheck).Format(time.RFC3339Nano)},
				}
			}
		}
		return c
	}
	fast := &podman.Healthcheck{Interval: 2 * time.Second, Timeout: 30 * time.Second, Retries: 2}
	defaults := &podman.Healthcheck{}
	for _, tc := range []struct {
		name string
		in   podman.Container
		want string
	}{
		{"recent check", inspect("healthy", fast, 3*time.Second), "healthy"},
		{"overdue", inspect("healthy", fast, 40*time.Second), "stalled"},
		{"overdue past retries", inspect("healthy", fast, 70*time.Second), "unhealthy"},
		{"defaults recent", inspect("healthy", defaults, 60*time.Second), "healthy"},
		{"defaults overdue", inspect("healthy", defaults, 70*time.Second), "stalled"},
		{"defaults past retries", inspect("healthy", defaults, 190*time.Second), "unhealthy"},
		{"never ran: no systemd session, keep the linger warning", inspect("starting", fast, 0), "starting"},
		{"unhealthy stays", inspect("unhealthy", fast, 3*time.Second), "unhealthy"},
		{"no healthcheck", inspect("", nil, 0), ""},
	} {
		if got := health(tc.in, now); got != tc.want {
			t.Errorf("%s: health = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Podman 4.9 matches no event at all on a label filter without a value, so
// anastasis asks for container events and keeps its own here.
func TestEventFilters(t *testing.T) {
	if _, ok := eventFilters["label"]; ok {
		t.Fatal("label filter in the events request: Podman 4.9 then sends no events")
	}
	var ours, other podman.Event
	ours.Actor.Attributes = map[string]string{LabelProject: "p", "name": "p-web-1"}
	other.Actor.Attributes = map[string]string{"name": "unrelated"}
	if !Ours(ours) || Ours(other) {
		t.Fatalf("Ours(ours) = %v, Ours(other) = %v", Ours(ours), Ours(other))
	}
}

// Only events that can change what a pass sees wake the loop: healthchecks
// emit exec events (Docker) or a health_status per run (Podman).
func TestWakes(t *testing.T) {
	ev := func(id, action, health string) podman.Event {
		var e podman.Event
		e.Actor.ID, e.Action, e.HealthStatus = id, action, health
		return e
	}
	last := map[string]string{}
	for i, tc := range []struct {
		e    podman.Event
		want bool
	}{
		{ev("a", "start", ""), true},
		{ev("a", "exec_create: /bin/sh -c curl", ""), false},
		{ev("a", "exec_start: /bin/sh -c curl", ""), false},
		{ev("a", "exec_die", ""), false},
		{ev("a", "health_status", "healthy"), true},   // Podman
		{ev("a", "health_status", "healthy"), false},  // Podman repeats every check
		{ev("b", "health_status: healthy", ""), true}, // Docker
		{ev("a", "health_status", "unhealthy"), true},
		{ev("a", "die", ""), true},
		{ev("a", "oom", ""), true},
		{ev("a", "destroy", ""), true},
		{ev("b", "remove", ""), true}, // Podman's name for destroy
	} {
		if got := Wakes(tc.e, last); got != tc.want {
			t.Errorf("%d %s %s: Wakes = %v, want %v", i, tc.e.Action, tc.e.HealthStatus, got, tc.want)
		}
	}
	if len(last) != 0 {
		t.Fatalf("health of removed containers kept: %v", last)
	}
}
