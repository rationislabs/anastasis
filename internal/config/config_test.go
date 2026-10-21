package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
)

// loadAs writes files into a fresh directory and loads the one named file.
func loadAs(t *testing.T, file, body string, env []string, extra map[string]string) (*model.Project, error) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range extra {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(context.Background(), path, env)
}

func load(t *testing.T, yaml string, env ...string) (*model.Project, error) {
	t.Helper()
	return loadAs(t, "compose.yaml", yaml, env, nil)
}

func mustLoad(t *testing.T, yaml string, env ...string) *model.Project {
	t.Helper()
	p, err := load(t, yaml, env...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func loadFile(t *testing.T, rel string) *model.Project {
	t.Helper()
	path, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSpecExample(t *testing.T) {
	p := loadFile(t, "testdata/valid.yaml")
	if p.Name != "weather" {
		t.Fatalf("name = %q", p.Name)
	}
	web := p.Services["web"]
	if web.Replicas != 3 || !web.Sticky || web.Expose == nil || *web.Expose != (model.Expose{HostPort: 8000, ContainerPort: 8080}) {
		t.Fatalf("web = %+v", web)
	}
	wantUpd := model.UpdateConfig{Parallelism: 1, Delay: 5 * time.Second, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "pause"}
	if web.Update != wantUpd {
		t.Fatalf("web.Update = %+v, want %+v", web.Update, wantUpd)
	}
	if web.StopGrace != 10*time.Second || web.Hash == "" {
		t.Fatalf("web.StopGrace = %v, Hash = %q", web.StopGrace, web.Hash)
	}
	worker := p.Services["worker"]
	if worker.Replicas != 1 || worker.Expose != nil || worker.Update.Order != "start-first" {
		t.Fatalf("worker = %+v", worker)
	}
}

func TestLoadHomelabStyle(t *testing.T) {
	p := loadFile(t, "testdata/homelab/compose.yaml")
	if p.Name != "homelab" {
		t.Fatalf("name = %q, want directory name", p.Name)
	}
	app := p.Services["app"]
	if tz := app.Config.Environment["TZ"]; tz != "Europe/Bratislava" {
		t.Fatalf("env_file not applied: %v", app.Config.Environment)
	}
	var sources []string
	for _, v := range app.Config.Volumes {
		sources = append(sources, v.Source)
	}
	if sources[0] != "homelab_data" || !filepath.IsAbs(sources[1]) {
		t.Fatalf("volume sources = %v", sources)
	}
	if len(p.Volumes) != 1 || p.Volumes[0] != "homelab_data" {
		t.Fatalf("project volumes = %v", p.Volumes)
	}
}

func TestLoadJSON(t *testing.T) {
	p, err := loadAs(t, "compose.json", `{"name": "j", "services": {"web": {"image": "nginx", "deploy": {"replicas": 2}, "x-anastasis": {"expose": "8000:80"}}}}`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w := p.Services["web"]; w.Replicas != 2 || w.Expose == nil || w.Config.Image != "nginx" {
		t.Fatalf("web = %+v", w)
	}
}

func TestInterpolation(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx:${TAG}\n", "TAG=1.27")
	if img := p.Services["web"].Config.Image; img != "nginx:1.27" {
		t.Fatalf("image = %q", img)
	}
}

func TestDotEnvAndPrecedence(t *testing.T) {
	p, err := loadAs(t, "compose.yaml", "name: t\nservices:\n  web:\n    image: nginx:${TAG}\n    environment: [\"MODE=${MODE}\"]\n",
		[]string{"MODE=prod"}, map[string]string{".env": "TAG=1.27\nMODE=dev\n"})
	if err != nil {
		t.Fatal(err)
	}
	c := p.Services["web"].Config
	if c.Image != "nginx:1.27" || c.Environment["MODE"] != "prod" {
		t.Fatalf("image %q env %v: .env fills gaps, the process environment wins", c.Image, c.Environment)
	}
}

func TestInterpolatedScalars(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    read_only: ${RO:-true}\n    deploy:\n      replicas: ${N:-2}\n      resources: {limits: {cpus: \"${CPUS:-0.5}\"}}\n")
	w := p.Services["web"]
	if w.Replicas != 2 || !w.Config.ReadOnly || w.Config.Deploy.Resources.Limits.CPUs != 0.5 {
		t.Fatalf("replicas %d read_only %v limits %+v", w.Replicas, w.Config.ReadOnly, w.Config.Deploy.Resources.Limits)
	}
}

func TestShortAndLongSyntax(t *testing.T) {
	p, err := loadAs(t, "compose.yaml", `name: s
services:
  web:
    image: nginx
    command: nginx -g 'daemon off;'
    environment: ["A=1", "B=two words", FROM_ENV]
    labels: ["team=home", tier]
    ports: ["127.0.0.1:8080:80", "9090:90/udp"]
    volumes:
      - data:/data:ro
      - ./conf:/etc/conf:z
      - /cache
      - type: bind
        source: /srv/x
        target: /x
    healthcheck:
      test: wget -qO- http://localhost/ || exit 1
      retries: 2
    stop_grace_period: 1m30s
    deploy:
      resources: {limits: {cpus: 0.25, memory: 512m}}
volumes:
  data:
`, []string{"FROM_ENV=x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := p.Services["web"]
	c := w.Config
	if !reflect.DeepEqual(c.Command, []string{"nginx", "-g", "daemon off;"}) {
		t.Fatalf("command = %q", c.Command)
	}
	if !reflect.DeepEqual(c.Environment, map[string]string{"A": "1", "B": "two words", "FROM_ENV": "x"}) ||
		!reflect.DeepEqual(c.Labels, map[string]string{"team": "home", "tier": ""}) {
		t.Fatalf("env %v labels %v", c.Environment, c.Labels)
	}
	wantPorts := []model.Port{{HostIP: "127.0.0.1", Target: 80, Published: "8080", Protocol: "tcp"}, {Target: 90, Published: "9090", Protocol: "udp"}}
	if !reflect.DeepEqual(c.Ports, wantPorts) {
		t.Fatalf("ports = %+v", c.Ports)
	}
	dir := filepath.Dir(c.Volumes[1].Source)
	wantVols := []model.Volume{
		{Type: "volume", Source: "s_data", Target: "/data", ReadOnly: true},
		{Type: "bind", Source: filepath.Join(dir, "conf"), Target: "/etc/conf", Bind: &model.Bind{SELinux: "z"}},
		{Type: "volume", Target: "/cache"},
		{Type: "bind", Source: "/srv/x", Target: "/x"},
	}
	if !reflect.DeepEqual(c.Volumes, wantVols) {
		t.Fatalf("volumes = %+v", c.Volumes)
	}
	if hc := c.HealthCheck; hc == nil || !reflect.DeepEqual(hc.Test, []string{"CMD-SHELL", "wget -qO- http://localhost/ || exit 1"}) || hc.Retries != 2 {
		t.Fatalf("healthcheck = %+v", c.HealthCheck)
	}
	if w.StopGrace != 90*time.Second || c.Deploy.Resources.Limits.Memory != 512<<20 || c.Deploy.Resources.Limits.CPUs != 0.25 {
		t.Fatalf("grace %v limits %+v", w.StopGrace, c.Deploy.Resources.Limits)
	}
}

func TestYAMLAnchors(t *testing.T) {
	p := mustLoad(t, "name: t\nx-common: &c\n  image: nginx:1.27\n  labels: {team: home}\nservices:\n  web:\n    <<: *c\n  api:\n    <<: *c\n    image: nginx:1.28\n")
	if p.Services["web"].Config.Image != "nginx:1.27" || p.Services["api"].Config.Image != "nginx:1.28" || p.Services["api"].Config.Labels["team"] != "home" {
		t.Fatalf("services = %+v", p.Services)
	}
}

func TestProjectName(t *testing.T) {
	if p := mustLoad(t, "name: t\nservices:\n  web: {image: nginx}\n", "COMPOSE_PROJECT_NAME=fromenv"); p.Name != "fromenv" {
		t.Fatalf("name = %q, want COMPOSE_PROJECT_NAME to win", p.Name)
	}
	if p := mustLoad(t, "name: ${N}\nservices:\n  web: {image: nginx}\n", "N=x1"); p.Name != "x1" {
		t.Fatalf("name = %q", p.Name)
	}
	if p := mustLoad(t, "name: My Stack\nservices:\n  web: {image: nginx}\n"); p.Name != "mystack" {
		t.Fatalf("name = %q, want the file's name normalized as compose-go does", p.Name)
	}
	if _, err := load(t, "name: t\nservices:\n  web: {image: nginx}\n", "COMPOSE_PROJECT_NAME=Bad Name"); err == nil || !strings.Contains(err.Error(), "invalid project name") {
		t.Fatalf("err = %v", err)
	}
	dir := filepath.Join(t.TempDir(), "My.Stack_2")
	os.Mkdir(dir, 0o700)
	path := filepath.Join(dir, "compose.yaml")
	os.WriteFile(path, []byte("services:\n  web: {image: nginx}\n"), 0o600)
	p, err := Load(context.Background(), path, nil)
	if err != nil || p.Name != "mystack_2" {
		t.Fatalf("name = %v, %v; want the directory name lowercased with invalid characters dropped", p, err)
	}
}

func TestLoadReportsPosition(t *testing.T) {
	if _, err := load(t, "name: t\nservices:\n\tweb: {}\n"); err == nil || !strings.Contains(err.Error(), "compose.yaml:3:1: tabs") {
		t.Fatalf("yaml err = %v", err)
	}
	if _, err := loadAs(t, "compose.json", "{\n  \"name\": \"t\",\n  \"services\": {,}\n}", nil, nil); err == nil || !strings.Contains(err.Error(), "compose.json:3:") {
		t.Fatalf("json err = %v", err)
	}
	if _, err := load(t, "name: t\nservices:\n  web:\n    image: nginx\n    deploy: {replicas: many}\n"); err == nil || !strings.Contains(err.Error(), `service "web": deploy.replicas: want a whole number`) {
		t.Fatalf("type err = %v", err)
	}
}

func TestEnvFileOptional(t *testing.T) {
	p, err := loadAs(t, "compose.yaml", "name: t\nservices:\n  web:\n    image: nginx\n    env_file:\n      - path: missing.env\n        required: false\n      - app.env\n",
		nil, map[string]string{"app.env": "# c\nexport A=1\nB=\"x y\"\nC='z' \n"})
	if err != nil {
		t.Fatal(err)
	}
	if env := p.Services["web"].Config.Environment; !reflect.DeepEqual(env, map[string]string{"A": "1", "B": "x y", "C": "z"}) {
		t.Fatalf("env = %v", env)
	}
	if _, err := load(t, "name: t\nservices:\n  web:\n    image: nginx\n    env_file: missing.env\n"); err == nil || !strings.Contains(err.Error(), "env_file") {
		t.Fatalf("missing required env_file: err = %v", err)
	}
}

func TestPortsDefaultToStopFirst(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    ports: [\"8080:80\"]\n")
	if o := p.Services["web"].Update.Order; o != "stop-first" {
		t.Fatalf("order = %q", o)
	}
}

func TestHashIgnoresReplicas(t *testing.T) {
	a := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx:1\n    deploy: {replicas: 1}\n")
	b := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx:1\n    deploy: {replicas: 4}\n")
	if a.Services["web"].Hash != b.Services["web"].Hash {
		t.Fatal("replica count changed the hash")
	}
}

func TestRejects(t *testing.T) {
	const head = "name: t\nservices:\n  web:\n    image: nginx\n"
	tests := []struct{ name, yaml, want string }{
		{"build", "name: t\nservices:\n  web:\n    build: .\n", "build: is not supported"},
		{"no image", "name: t\nservices:\n  web:\n    user: x\n", "image: is required"},
		{"ports with replicas", head + "    ports: [\"80:80\"]\n    deploy: {replicas: 2}\n", "move it to x-anastasis.expose"},
		{"ports with start-first", head + "    ports: [\"80:80\"]\n    deploy: {update_config: {order: start-first}}\n", "start-first"},
		{"port range", head + "    ports: [\"8000-8001:80-81\"]\n", "port ranges are not supported"},
		{"network_mode", head + "    network_mode: host\n", "network_mode"},
		{"placement", head + "    deploy: {placement: {constraints: [\"node.role==manager\"]}}\n", "deploy.placement"},
		{"global mode", head + "    deploy: {mode: global}\n", "deploy.mode"},
		{"endpoint_mode", head + "    deploy: {endpoint_mode: vip}\n", "deploy.endpoint_mode"},
		{"rollback_config", head + "    deploy: {rollback_config: {parallelism: 1}}\n", "deploy.rollback_config"},
		{"restart_policy none", head + "    deploy: {restart_policy: {condition: none}}\n", "deploy.restart_policy.condition"},
		{"max_failure_ratio", head + "    deploy: {update_config: {max_failure_ratio: 0.5}}\n", "max_failure_ratio"},
		{"bad failure_action", head + "    deploy: {update_config: {failure_action: explode}}\n", "failure_action"},
		{"sticky without expose", head + "    x-anastasis: {sticky: true}\n", "x-anastasis.sticky needs x-anastasis.expose"},
		{"bad expose", head + "    x-anastasis: {expose: abc}\n", "x-anastasis.expose"},
		{"unknown x-anastasis key", head + "    x-anastasis: {replica: 2}\n", "x-anastasis"},
		{"unsupported key", head + "    privileged: true\n", "privileged: is not supported"},
		{"unsupported top-level key", "name: t\nsecrets: {}\nservices:\n  web: {image: nginx}\n", "secrets: is not supported"},
		{"custom network", head + "    networks: [back]\nnetworks:\n  back: {}\n", "custom networks"},
		{"restart no", head + "    restart: \"no\"\n", "restart: no is not supported"},
		{"restart on-failure", head + "    restart: on-failure\n", "restart: on-failure is not supported"},
		{"bind propagation", head + "    volumes:\n      - type: bind\n        source: /tmp\n        target: /x\n        bind: {propagation: rshared}\n", "propagation"},
		{"volume nocopy", head + "    volumes:\n      - type: volume\n        source: data\n        target: /d\n        volume: {nocopy: true}\nvolumes:\n  data: {}\n", "nocopy"},
		{"tmpfs options", head + "    volumes:\n      - type: tmpfs\n        target: /t\n        tmpfs: {size: 1000}\n", "tmpfs options"},
		{"undefined volume", head + "    volumes: [\"nope:/x\"]\n", "undefined volume nope"},
		{"volume driver", head + "    volumes: [\"d:/x\"]\nvolumes:\n  d: {driver: nfs}\n", "driver"},
		{"duplicate expose port", head + "    x-anastasis: {expose: \"8000:80\"}\n  api:\n    image: nginx\n    x-anastasis: {expose: \"8000:80\"}\n", "both expose host port 8000"},
		{"required variable", "name: t\nservices:\n  web:\n    image: \"nginx:${TAG:?set TAG}\"\n", "set TAG"},
		{"empty file", "", "empty project file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.yaml)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestExternalVolumesNotOwned(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    volumes: [\"data:/d\", \"media:/m\", \"c:/c\"]\nvolumes:\n  data: {}\n  media: {external: true}\n  c: {name: my-cache}\n")
	if !reflect.DeepEqual(p.Volumes, []string{"my-cache", "t_data"}) {
		t.Fatalf("project volumes = %v; external volumes must never be owned (down --volumes deletes them)", p.Volumes)
	}
	if src := p.Services["web"].Config.Volumes[1].Source; src != "media" {
		t.Fatalf("external volume source = %q", src)
	}
}

func TestLoadAutoscale(t *testing.T) {
	p := mustLoad(t, `name: t
services:
  web:
    image: nginx
    deploy:
      replicas: 2
      resources: {limits: {cpus: "0.5"}}
    x-anastasis:
      expose: "8000:80"
      autoscale: {max: 6, inflight: 10, cpu: 70}
`)
	web := p.Services["web"]
	want := model.Autoscale{Min: 2, Max: 6, Inflight: 10, CPU: 70, CPULimit: 0.5}
	if web.Autoscale == nil || *web.Autoscale != want || web.Replicas != 2 {
		t.Fatalf("autoscale = %+v, replicas %d; want %+v (min defaults to replicas)", web.Autoscale, web.Replicas, want)
	}
}

func TestAutoscaleClampsReplicas(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    deploy: {replicas: 8}\n    x-anastasis:\n      expose: \"8000:80\"\n      autoscale: {min: 1, max: 4, inflight: 5}\n")
	if n := p.Services["web"].Replicas; n != 4 {
		t.Fatalf("replicas = %d, want clamped to max 4", n)
	}
}

func TestAutoscaleRejects(t *testing.T) {
	const head = "name: t\nservices:\n  web:\n    image: nginx\n"
	const exposed = head + "    x-anastasis:\n      expose: \"8000:80\"\n"
	tests := []struct{ name, yaml, want string }{
		{"no max", exposed + "      autoscale: {inflight: 5}\n", "max is required"},
		{"min zero", exposed + "      autoscale: {min: 0, max: 4, inflight: 5}\n", "min must be >= 1"},
		{"replicas zero as default min", head + "    deploy: {replicas: 0}\n    x-anastasis:\n      expose: \"8000:80\"\n      autoscale: {max: 4, inflight: 5}\n", "min must be >= 1"},
		{"min above max", exposed + "      autoscale: {min: 5, max: 2, inflight: 5}\n", "greater than max"},
		{"no signal", exposed + "      autoscale: {max: 4}\n", "set inflight, cpu or both"},
		{"negative target", exposed + "      autoscale: {max: 4, inflight: -1}\n", "must be positive"},
		{"inflight without expose", head + "    x-anastasis:\n      autoscale: {max: 4, inflight: 5}\n", "inflight needs x-anastasis.expose"},
		{"cpu without limit", exposed + "      autoscale: {max: 4, cpu: 70}\n", "cpu needs deploy.resources.limits.cpus"},
		{"ports", head + "    ports: [\"80:80\"]\n    deploy: {resources: {limits: {cpus: \"1\"}}}\n    x-anastasis:\n      autoscale: {max: 4, cpu: 70}\n", "ports:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.yaml)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestNumbersKeepText(t *testing.T) {
	want := map[string]string{"UMASK": "022", "BIG": "12345678901234567890", "V": "1.10"}
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    environment: {UMASK: 022, BIG: 12345678901234567890, V: 1.10}\n")
	if env := p.Services["web"].Config.Environment; !reflect.DeepEqual(env, want) {
		t.Fatalf("yaml env = %v, want the values as written", env)
	}
	p, err := loadAs(t, "compose.json", `{"name": "t", "services": {"web": {"image": "nginx", "environment": {"UMASK": "022", "BIG": 12345678901234567890, "V": 1.10}}}}`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if env := p.Services["web"].Config.Environment; !reflect.DeepEqual(env, want) {
		t.Fatalf("json env = %v", env)
	}
}

func TestEmptyEntrypointClearsImageDefault(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    entrypoint: []\n    command: \"\"\n  api:\n    image: nginx\n")
	if c := p.Services["web"].Config; c.Entrypoint == nil || len(c.Entrypoint) != 0 || c.Command == nil || len(c.Command) != 0 {
		t.Fatalf("entrypoint %#v command %#v, want set-but-empty", c.Entrypoint, c.Command)
	}
	if c := p.Services["api"].Config; c.Entrypoint != nil || c.Command != nil {
		t.Fatalf("unset entrypoint %#v command %#v, want nil (image default)", c.Entrypoint, c.Command)
	}
}

func TestDotEnvExpands(t *testing.T) {
	p, err := loadAs(t, "compose.yaml", "name: t\nservices:\n  web:\n    image: nginx:${TAG}\n",
		[]string{"BASE=1"}, map[string]string{".env": "TAG=${BASE}.27\n"})
	if err != nil || p.Services["web"].Config.Image != "nginx:1.27" {
		t.Fatalf("image = %v, %v; .env values expand ${VAR} from the environment", p, err)
	}
}

func TestAutoscaleParallelismZeroIsAllAtOnce(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    deploy:\n      replicas: 2\n      update_config: {parallelism: 0}\n    x-anastasis:\n      expose: \"8000:80\"\n      autoscale: {max: 6, inflight: 5}\n")
	if n := p.Services["web"].Update.Parallelism; n != 6 {
		t.Fatalf("parallelism = %d, want 6: 0 means all at once, and an autoscaled service can grow to max", n)
	}
}

func TestJSONErrorsHavePosition(t *testing.T) {
	tests := []struct{ name, json, want string }{
		{"empty", "", "compose.json:1:1: unexpected end of JSON input"},
		{"truncated", "{\"name\": \"t\",\n  \"services\": {\n", "compose.json:2:16: unexpected end of JSON input"},
		{"trailing data", "{\"name\": \"t\"}\n\n x\n", "compose.json:3:2: unexpected data after the top-level value"},
		{"duplicate key", "{\"name\": \"t\",\n \"services\": {\"web\": {\"image\": \"a\", \"image\": \"b\"}}}", "compose.json:2:37: duplicate key \"image\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadAs(t, "compose.json", tt.json, nil, nil)
			if err == nil || !strings.HasSuffix(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to end with %q", err, tt.want)
			}
		})
	}
}

func TestBooleanSpellings(t *testing.T) {
	for in, want := range map[string]bool{"yes": true, "\"NO\"": false, "On": true, "off": false, "y": true, "N": false, "\"True\"": true} {
		p, err := load(t, "name: t\nservices:\n  web:\n    image: nginx\n    read_only: "+in+"\n")
		if err != nil || p.Services["web"].Config.ReadOnly != want {
			t.Errorf("read_only: %s = %v, %v; want %v", in, p, err, want)
		}
	}
	if _, err := load(t, "name: t\nservices:\n  web:\n    image: nginx\n    read_only: maybe\n"); err == nil || !strings.Contains(err.Error(), "want true or false") {
		t.Errorf("maybe: err = %v", err)
	}
}

func TestDurationDaysAndWeeks(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    stop_grace_period: 1d\n    healthcheck:\n      test: [\"CMD\", \"true\"]\n      interval: 1w2d3h30m\n")
	w := p.Services["web"]
	if w.StopGrace != 24*time.Hour || time.Duration(w.Config.HealthCheck.Interval) != 9*24*time.Hour+3*time.Hour+30*time.Minute {
		t.Fatalf("grace %v interval %v", w.StopGrace, time.Duration(w.Config.HealthCheck.Interval))
	}
	for _, bad := range []string{"d", "1x", "\"\"", "1d2w"} {
		if _, err := load(t, "name: t\nservices:\n  web:\n    image: nginx\n    stop_grace_period: "+bad+"\n"); err == nil || !strings.Contains(err.Error(), "want a duration") {
			t.Errorf("stop_grace_period %s: err = %v", bad, err)
		}
	}
}

func TestPortProtocol(t *testing.T) {
	p := mustLoad(t, "name: t\nservices:\n  web:\n    image: nginx\n    ports:\n      - 8080:80/UDP\n      - {target: 90, protocol: SCTP}\n")
	if ps := p.Services["web"].Config.Ports; ps[0].Protocol != "udp" || ps[1].Protocol != "sctp" {
		t.Fatalf("ports = %+v, want protocols lowercased", ps)
	}
	for _, port := range []string{"80/bogus", "{target: 80, protocol: http}"} {
		if _, err := load(t, "name: t\nservices:\n  web:\n    image: nginx\n    ports: ["+port+"]\n"); err == nil || !strings.Contains(err.Error(), "tcp, udp or sctp") {
			t.Errorf("%s: err = %v", port, err)
		}
	}
}

func TestDependsOnUndefinedService(t *testing.T) {
	const head = "name: t\nservices:\n  web:\n    image: nginx\n"
	for _, deps := range []string{"[db]", "{db: {condition: service_started}}"} {
		if _, err := load(t, head+"    depends_on: "+deps+"\n"); err == nil || !strings.Contains(err.Error(), `service "web": depends_on: undefined service "db"`) {
			t.Errorf("depends_on %s: err = %v", deps, err)
		}
		if _, err := load(t, head+"    depends_on: "+deps+"\n  db:\n    image: postgres\n"); err != nil {
			t.Errorf("depends_on %s with db defined: %v", deps, err)
		}
	}
}

func TestNestedAliasesAreCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("name: t\nx-l0: &l0 [lol, lol]\n")
	for i := 1; i <= 22; i++ {
		fmt.Fprintf(&b, "x-l%d: &l%d [*l%d, *l%d]\n", i, i, i-1, i-1)
	}
	b.WriteString("services:\n  web:\n    image: nginx\n")
	start := time.Now()
	_, err := load(t, b.String())
	if err == nil || !strings.Contains(err.Error(), "aliases") {
		t.Fatalf("err = %v, want the expansion rejected", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v; nested aliases must be rejected before they expand", d)
	}
}
