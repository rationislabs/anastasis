package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/config"
	"github.com/rationislabs/anastasis/internal/model"
)

func project(image string) model.Project {
	svc := model.Service{Project: "p", Name: "web", Replicas: 1, Config: model.Config{Image: image}}
	svc.Hash = model.ConfigHash(svc)
	return model.Project{Name: "p", Services: map[string]model.Service{"web": svc}}
}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "anastasis", "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestRoundTripKeepsHash(t *testing.T) {
	src, _ := filepath.Abs("../config/testdata/valid.yaml")
	p, err := config.Load(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, path := open(t)
	if err := s.Up(*p, "/c.yaml"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, ok := s2.Get("weather")
	if !ok {
		t.Fatal("project lost")
	}
	for name, svc := range got.Current.Services {
		if h := model.ConfigHash(svc); h != svc.Hash || h != p.Services[name].Hash {
			t.Errorf("%s: hash after reload %s, stored %s, original %s", name, h, svc.Hash, p.Services[name].Hash)
		}
	}
}

func TestSecondOpenFails(t *testing.T) {
	_, path := open(t)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "another anastasis serve") {
		t.Fatalf("err = %v", err)
	}
}

func TestCorruptFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(path, []byte("{"), 0o600)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "{" {
		t.Fatalf("file changed to %q", b)
	}
}

func TestUpKeepsPreviousAndSalt(t *testing.T) {
	s, _ := open(t)
	s.Up(project("nginx:1"), "/c.yaml")
	s.Scale("p", map[string]int{"web": 4})
	s.Restart("p", "web")
	salt := mustGet(t, s).Salt["web"]
	if err := s.Up(project("nginx:2"), "/c.yaml"); err != nil {
		t.Fatal(err)
	}
	ps := mustGet(t, s)
	if ps.Previous == nil || ps.Previous.Services["web"].Config.Image != "nginx:1" {
		t.Fatalf("previous = %+v", ps.Previous)
	}
	if ps.Salt["web"] != salt || salt == "" {
		t.Fatalf("salt = %q, want %q", ps.Salt["web"], salt)
	}
	if _, ok := ps.Scale["web"]; ok {
		t.Fatal("scale override survived up")
	}
}

func TestScaleErrors(t *testing.T) {
	s, _ := open(t)
	s.Up(project("nginx:1"), "/c.yaml")
	for _, tt := range []struct {
		project string
		scale   map[string]int
		want    string
	}{
		{"nope", map[string]int{"web": 1}, "not registered"},
		{"p", map[string]int{"api": 1}, "no service"},
		{"p", map[string]int{"web": -1}, ">= 0"},
	} {
		if err := s.Scale(tt.project, tt.scale); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Scale(%s, %v) = %v, want %q", tt.project, tt.scale, err, tt.want)
		}
	}
	withPorts := project("nginx:1")
	web := withPorts.Services["web"]
	web.Config.Ports = []model.Port{{Target: 80, Published: "80"}}
	withPorts.Services["web"] = web
	s.Up(withPorts, "/c.yaml")
	if err := s.Scale("p", map[string]int{"web": 2}); err == nil || !strings.Contains(err.Error(), "ports:") {
		t.Errorf("scaling a ports: service to 2 = %v", err)
	}
}

func TestRollback(t *testing.T) {
	s, _ := open(t)
	s.Up(project("nginx:1"), "/c.yaml")
	s.Up(project("nginx:1"), "/c.yaml")
	if ok, _ := s.Rollback("p", "web"); ok {
		t.Fatal("rolled back to an identical config")
	}
	s.Up(project("nginx:2"), "/c.yaml")
	ok, err := s.Rollback("p", "web")
	if err != nil || !ok {
		t.Fatalf("Rollback = %v, %v", ok, err)
	}
	ps := mustGet(t, s)
	if ps.Current.Services["web"].Config.Image != "nginx:1" || ps.Previous.Services["web"].Config.Image != "nginx:2" {
		t.Fatalf("after rollback current=%s previous=%s", ps.Current.Services["web"].Config.Image, ps.Previous.Services["web"].Config.Image)
	}
}

func TestFailedSaveKeepsMemory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, path := open(t)
	os.Chmod(filepath.Dir(path), 0o500)
	defer os.Chmod(filepath.Dir(path), 0o700)
	if err := s.Up(project("nginx:1"), "/c.yaml"); err == nil {
		t.Fatal("Up succeeded on a read-only directory")
	}
	if _, ok := s.Get("p"); ok {
		t.Fatal("failed Up changed memory")
	}
}

func mustGet(t *testing.T, s *Store) ProjectState {
	t.Helper()
	ps, ok := s.Get("p")
	if !ok {
		t.Fatal("project p missing")
	}
	return ps
}

func TestUpRefusesSameNameFromOtherFile(t *testing.T) {
	s, _ := open(t)
	if err := s.Up(project("nginx:1"), "/stacks/ntfy/docker/compose.yaml"); err != nil {
		t.Fatal(err)
	}
	err := s.Up(project("nginx:2"), "/stacks/gitea/docker/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "/stacks/ntfy/docker/compose.yaml") {
		t.Fatalf("err = %v, want a refusal naming the first file", err)
	}
	if img := mustGet(t, s).Current.Services["web"].Config.Image; img != "nginx:1" {
		t.Fatalf("project replaced: image %s", img)
	}
}
func TestScaleRejectsAutoscaled(t *testing.T) {
	s, _ := open(t)
	p := project("nginx:1")
	web := p.Services["web"]
	web.Autoscale = &model.Autoscale{Min: 1, Max: 4, Inflight: 5}
	p.Services["web"] = web
	s.Up(p, "/c.yaml")
	if err := s.Scale("p", map[string]int{"web": 3}); err == nil || !strings.Contains(err.Error(), "autoscaled") {
		t.Fatalf("Scale on an autoscaled service = %v", err)
	}
}

// TestLoadsStateFromComposeGoBuild opens a state file written before
// sub-project D, when Config was compose-go's type. Nothing may need a
// manual step: values decode, stored hashes stay, binds still auto-create.
func TestLoadsStateFromComposeGoBuild(t *testing.T) {
	b, err := os.ReadFile("testdata/state-compose-go.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]struct {
		Current struct {
			Services map[string]struct{ Hash string }
		}
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	spec, _ := s.Get("spec")
	web := spec.Current.Services["web"]
	c := web.Config
	if c.Image != "docker.io/library/nginx:1.27-alpine" || !reflect.DeepEqual(c.Command, []string{"nginx", "-g", "daemon off;"}) ||
		c.Environment["A"] != "1" || c.Labels["team"] != "home" || !reflect.DeepEqual(c.CapDrop, []string{"ALL"}) {
		t.Fatalf("spec web config = %+v", c)
	}
	if c.HealthCheck == nil || c.HealthCheck.Interval != model.Duration(5*time.Second) || c.HealthCheck.Retries != 3 {
		t.Fatalf("healthcheck = %+v", c.HealthCheck)
	}
	if c.Deploy == nil || c.Deploy.Resources.Limits == nil || c.Deploy.Resources.Limits.CPUs != 0.5 || c.Deploy.Resources.Limits.Memory != 256<<20 {
		t.Fatalf("limits = %+v", c.Deploy)
	}
	if web.Hash != raw["spec"].Current.Services["web"].Hash {
		t.Fatalf("stored hash changed on load: %s", web.Hash)
	}

	home, _ := s.Get("homelab")
	vols := home.Current.Services["app"].Config.Volumes
	if len(vols) != 2 || vols[0].Source != "homelab_data" || vols[0].CreatesSource() || !vols[1].CreatesSource() || !vols[1].ReadOnly {
		t.Fatalf("volumes = %+v (short-syntax binds must still auto-create)", vols)
	}
	if home.Current.Services["app"].Config.Environment["TZ"] != "Europe/Bratislava" {
		t.Fatalf("env = %v", home.Current.Services["app"].Config.Environment)
	}
}

// Switching a project from compose.yaml to compose.json in the same
// directory is the same project, not a clash.
func TestUpAcceptsOtherFileInSameDirectory(t *testing.T) {
	s, _ := open(t)
	s.Up(project("nginx:1"), "/stacks/round2/compose.yaml")
	if err := s.Up(project("nginx:2"), "/stacks/round2/compose.json"); err != nil {
		t.Fatal(err)
	}
	ps := mustGet(t, s)
	if ps.Source != "/stacks/round2/compose.json" || ps.Previous == nil {
		t.Fatalf("source %q, previous %v; want the new file and the old config kept", ps.Source, ps.Previous)
	}
}
