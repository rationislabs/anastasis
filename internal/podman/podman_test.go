package podman

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type seen struct {
	method, path string
	query        map[string]string
	body         []byte
}

// fake serves one canned answer on a unix socket. Each request it gets is
// sent on the returned channel (a channel, not a shared struct, so the race
// detector sees the hand-over).
func fake(t *testing.T, status int, answer string) (*Client, <-chan seen) {
	t.Helper()
	return fakeWith(t, status, answer, `{"ApiVersion":"1.41","MinAPIVersion":"1.24"}`)
}

// fakeWith also answers GET /version with version ("" answers 404). Version
// calls are not sent on the channel.
func fakeWith(t *testing.T, status int, answer, version string) (*Client, <-chan seen) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pm") // short path: unix socket names max out near 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	reqs := make(chan seen, 10)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			if version == "" {
				w.WriteHeader(404)
				return
			}
			io.WriteString(w, version)
			return
		}
		got := seen{method: r.Method, path: r.URL.Path, query: map[string]string{}}
		for k, v := range r.URL.Query() {
			got.query[k] = v[0]
		}
		got.body, _ = io.ReadAll(r.Body)
		reqs <- got
		w.WriteHeader(status)
		io.WriteString(w, answer)
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return New(sock), reqs
}

var ctx = context.Background()

func TestListContainers(t *testing.T) {
	c, reqs := fake(t, 200, `[{"Id":"a","Names":["/x"]},{"Id":"b"}]`)
	ids, err := c.ListContainers(ctx, "anastasis.project")
	if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	if got := <-reqs; got.method != "GET" || got.path != "/v1.41/containers/json" || got.query["all"] != "true" || got.query["filters"] != `{"label":["anastasis.project"]}` {
		t.Fatalf("request = %+v", got)
	}
}

// inspectAnswer has the shape of a Podman 4.9 inspect answer, trimmed to
// the fields anastasis reads.
const inspectAnswer = `{"Id":"abc","Name":"/web-1","Config":{"Labels":{"anastasis.project":"p"},
"Healthcheck":{"Test":["CMD-SHELL","true"],"Interval":2000000000,"Timeout":30000000000,"Retries":3}},
"State":{"Status":"running","StartedAt":"2026-10-14T10:00:00.5Z","FinishedAt":"0001-01-01T00:00:00Z",
"Health":{"Status":"healthy","FailingStreak":0,"Log":[{"Start":"2026-10-14T10:00:02.17Z","End":"2026-10-14T10:00:02.26Z","ExitCode":0,"Output":""}]}},
"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"32768"}],"81/tcp":null}}}`

func TestInspectContainer(t *testing.T) {
	c, reqs := fake(t, 200, inspectAnswer)
	in, err := c.InspectContainer(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.path != "/v1.41/containers/abc/json" {
		t.Fatalf("path = %s", got.path)
	}
	if in.ID != "abc" || in.Name != "/web-1" || in.Config.Labels["anastasis.project"] != "p" ||
		in.State.Status != "running" || in.State.Health == nil || in.State.Health.Status != "healthy" ||
		in.NetworkSettings.Ports["80/tcp"][0] != (PortBinding{HostIP: "127.0.0.1", HostPort: "32768"}) ||
		in.Config.Healthcheck == nil || in.Config.Healthcheck.Interval != 2*time.Second || in.Config.Healthcheck.Retries != 3 ||
		len(in.State.Health.Log) != 1 || in.State.Health.Log[0].Start != "2026-10-14T10:00:02.17Z" {
		t.Fatalf("inspect = %+v", in)
	}
}

func TestNotFound(t *testing.T) {
	c, _ := fake(t, 404, `{"cause":"no such container","message":"no such container abc","response":404}`)
	_, err := c.InspectContainer(ctx, "abc")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "no such container abc") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerError(t *testing.T) {
	c, _ := fake(t, 500, `{"message":"boom"}`)
	err := c.RemoveVolume(ctx, "v")
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	c, _ = fake(t, 502, "bad gateway")
	if err := c.RemoveVolume(ctx, "v"); err == nil || !strings.Contains(err.Error(), "bad gateway") {
		t.Fatalf("plain-text error body: %v", err)
	}
}

func TestCreateContainer(t *testing.T) {
	c, reqs := fake(t, 201, `{"Id":"new1","Warnings":[]}`)
	spec := CreateSpec{
		Image: "nginx", Env: []string{"A=1"}, Labels: map[string]string{"k": "v"},
		Healthcheck:  &Healthcheck{Test: []string{"NONE"}},
		ExposedPorts: map[string]struct{}{"80/tcp": {}},
		HostConfig: HostConfig{NetworkMode: "anastasis-p", NanoCPUs: 5e8,
			Mounts:       []Mount{{Type: "volume", Source: "p_data", Target: "/data"}},
			PortBindings: map[string][]PortBinding{"80/tcp": {{HostIP: "127.0.0.1"}}}},
		NetworkingConfig: NetworkingConfig{EndpointsConfig: map[string]EndpointSettings{"anastasis-p": {Aliases: []string{"web"}}}},
	}
	id, err := c.CreateContainer(ctx, "p-web-1", spec)
	if err != nil || id != "new1" {
		t.Fatalf("id = %q, %v", id, err)
	}
	got := <-reqs
	if got.method != "POST" || got.path != "/v1.41/containers/create" || got.query["name"] != "p-web-1" {
		t.Fatalf("request = %+v", got)
	}
	var body map[string]any
	json.Unmarshal(got.body, &body)
	host := body["HostConfig"].(map[string]any)
	if body["Image"] != "nginx" || host["NetworkMode"] != "anastasis-p" || host["NanoCpus"] != 5e8 ||
		host["PortBindings"].(map[string]any)["80/tcp"].([]any)[0].(map[string]any)["HostIp"] != "127.0.0.1" {
		t.Fatalf("body = %s", got.body)
	}
	if v, ok := body["Cmd"]; ok && v != nil {
		t.Fatalf("Cmd sent although unset (would override the image's CMD): %s", got.body)
	}
	if _, err := c.CreateContainer(ctx, "x", CreateSpec{Image: "nginx", Entrypoint: []string{}}); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; !strings.Contains(string(got.body), `"Entrypoint":[]`) {
		t.Fatalf("entrypoint: [] must reach the engine as [] to clear the image's ENTRYPOINT: %s", got.body)
	}
}

func TestNegotiatesMinimumVersion(t *testing.T) {
	c, reqs := fakeWith(t, 200, `[]`, `{"ApiVersion":"1.52","MinAPIVersion":"1.44"}`)
	if _, err := c.ListContainers(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.path != "/v1.44/containers/json" {
		t.Fatalf("path = %s; Docker 29 refuses API versions below its minimum", got.path)
	}
	c, reqs = fakeWith(t, 200, `[]`, "")
	if _, err := c.ListContainers(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.path != "/v1.41/containers/json" {
		t.Fatalf("path = %s, want the 1.41 default when /version is unavailable", got.path)
	}
}

func TestStartStopAlreadyDone(t *testing.T) {
	c, reqs := fake(t, 304, "")
	if err := c.StartContainer(ctx, "x"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := <-reqs; got.path != "/v1.41/containers/x/start" || got.method != "POST" {
		t.Fatalf("start: %+v", got)
	}
	if err := c.StopContainer(ctx, "x", 10*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := <-reqs; got.path != "/v1.41/containers/x/stop" || got.query["t"] != "10" {
		t.Fatalf("stop: %+v", got)
	}
}

// A grace period under a second must not become an immediate kill.
func TestStopGraceRoundsUp(t *testing.T) {
	c, reqs := fake(t, 204, "")
	for _, tc := range []struct {
		grace time.Duration
		want  string
	}{{0, "0"}, {500 * time.Millisecond, "1"}, {1500 * time.Millisecond, "2"}, {10 * time.Second, "10"}} {
		if err := c.StopContainer(ctx, "x", tc.grace); err != nil {
			t.Fatal(err)
		}
		if got := <-reqs; got.query["t"] != tc.want {
			t.Errorf("grace %s: t = %q, want %q", tc.grace, got.query["t"], tc.want)
		}
	}
}

func TestRemoveContainer(t *testing.T) {
	c, reqs := fake(t, 204, "")
	if err := c.RemoveContainer(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.method != "DELETE" || got.path != "/v1.41/containers/x" || got.query["force"] != "true" || got.query["v"] != "true" {
		t.Fatalf("%+v", got)
	}
}

func frame(stream byte, s string) string {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return string(h) + s
}

func TestLogs(t *testing.T) {
	c, reqs := fake(t, 200, frame(1, "hello\n")+frame(2, "oops\n"))
	s, err := c.Logs(ctx, "x", 20)
	if err != nil || s != "hello\noops\n" {
		t.Fatalf("logs = %q, %v", s, err)
	}
	if got := <-reqs; got.query["tail"] != "20" || got.query["stdout"] != "true" || got.query["stderr"] != "true" {
		t.Fatalf("%+v", got)
	}
	c, _ = fake(t, 200, "raw tty text\n")
	if s, _ := c.Logs(ctx, "x", 20); s != "raw tty text\n" {
		t.Fatalf("tty logs = %q", s)
	}
}

func TestImageExists(t *testing.T) {
	c, reqs := fake(t, 200, `{"Id":"sha256:1"}`)
	if ok, err := c.ImageExists(ctx, "docker.io/library/nginx:1.27"); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if got := <-reqs; got.path != "/v1.41/images/docker.io/library/nginx:1.27/json" {
		t.Fatalf("%+v", got)
	}
	c, _ = fake(t, 404, `{"message":"no such image"}`)
	if ok, err := c.ImageExists(ctx, "nginx"); ok || err != nil {
		t.Fatalf("missing image: %v %v", ok, err)
	}
}

func TestPull(t *testing.T) {
	c, reqs := fake(t, 200, `{"status":"Pulling"}`+"\n"+`{"status":"Done"}`)
	if err := c.Pull(ctx, "docker.io/library/nginx:1.27"); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.method != "POST" || got.path != "/v1.41/images/create" || got.query["fromImage"] != "docker.io/library/nginx" || got.query["tag"] != "1.27" {
		t.Fatalf("%+v", got)
	}
	c, _ = fake(t, 200, `{"status":"Pulling"}`+"\n"+`{"error":"manifest unknown"}`)
	if err := c.Pull(ctx, "nginx:nope"); err == nil || err.Error() != "manifest unknown" {
		t.Fatalf("error inside a 200 stream: %v", err)
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"nginx":                    {"nginx", "latest"}, // an empty tag would pull every tag
		"nginx:1.27":               {"nginx", "1.27"},
		"localhost:5000/app":       {"localhost:5000/app", "latest"},
		"localhost:5000/app:2":     {"localhost:5000/app", "2"},
		"ghcr.io/a/b@sha256:abc":   {"ghcr.io/a/b", "sha256:abc"},
		"ghcr.io/a/b:1@sha256:abc": {"ghcr.io/a/b", "sha256:abc"},
	} {
		if n, tag := splitRef(ref); n != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q) = %q, %q; want %q", ref, n, tag, want)
		}
	}
}

func TestNetworks(t *testing.T) {
	c, reqs := fake(t, 404, `{"message":"network not found"}`)
	if ok, err := c.NetworkExists(ctx, "n"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if got := <-reqs; got.path != "/v1.41/networks/n" {
		t.Fatalf("%+v", got)
	}
	c, reqs = fake(t, 201, `{"Id":"n1"}`)
	if err := c.CreateNetwork(ctx, "n", map[string]string{"anastasis.project": "p"}); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.path != "/v1.41/networks/create" || string(got.body) != `{"Name":"n","Labels":{"anastasis.project":"p"}}` {
		t.Fatalf("%+v %s", got, got.body)
	}
	c, reqs = fake(t, 204, "")
	if err := c.RemoveNetwork(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.method != "DELETE" || got.path != "/v1.41/networks/n" {
		t.Fatalf("%+v", got)
	}
}

func TestRemoveVolume(t *testing.T) {
	c, reqs := fake(t, 204, "")
	if err := c.RemoveVolume(ctx, "p_data"); err != nil {
		t.Fatal(err)
	}
	if got := <-reqs; got.method != "DELETE" || got.path != "/v1.41/volumes/p_data" {
		t.Fatalf("%+v", got)
	}
}

func TestEvents(t *testing.T) {
	c, reqs := fake(t, 200, `{"Type":"container","Action":"die","Actor":{"ID":"a","Attributes":{"anastasis.project":"p"}}}`+"\n"+
		`{"Type":"container","Action":"start","Actor":{"ID":"b"}}`+"\n")
	msgs, errs := c.Events(ctx, map[string][]string{"type": {"container"}})
	for _, want := range []string{"die", "start"} {
		select {
		case e := <-msgs:
			if e.Action != want {
				t.Fatalf("event = %+v, want %s", e, want)
			}
		case err := <-errs:
			t.Fatal(err)
		}
	}
	if err := <-errs; err == nil {
		t.Fatal("stream end not reported")
	}
	if got := <-reqs; got.query["filters"] != `{"type":["container"]}` {
		t.Fatalf("query = %v", got.query)
	}
}

func TestUsage(t *testing.T) {
	c, reqs := fake(t, 200, `{"read":"2026-10-09T12:00:01.5Z","cpu_stats":{"cpu_usage":{"total_usage":7}},"memory_stats":{"usage":1048576,"limit":2097152}}`)
	u, err := c.Usage(ctx, "x")
	read := time.Date(2026, 10, 9, 12, 0, 1, 5e8, time.UTC)
	if err != nil || u != (Usage{CPU: 7, Memory: 1 << 20, MemLimit: 2 << 20, Read: read}) {
		t.Fatalf("Usage = %+v, %v", u, err)
	}
	if got := <-reqs; got.path != "/v1.41/containers/x/stats" || got.query["stream"] != "false" {
		t.Fatalf("%+v", got)
	}
}

func TestInspectHealthLogDetail(t *testing.T) {
	c, _ := fake(t, 200, `{"Id":"a","State":{"Health":{"Status":"unhealthy","Log":[{"Start":"s","End":"e","ExitCode":1,"Output":"boom"}]}}}`)
	in, err := c.InspectContainer(ctx, "a")
	if err != nil || in.State.Health.Log[0] != (HealthLog{Start: "s", End: "e", ExitCode: 1, Output: "boom"}) {
		t.Fatalf("health log = %+v, %v", in.State.Health, err)
	}
}

// IDs reach the engine escaped, so one cannot change the request.
func TestIDEscaped(t *testing.T) {
	c, reqs := fake(t, 200, `{"Id":"a"}`)
	c.InspectContainer(ctx, "a?b")
	if got := <-reqs; got.path != "/v1.41/containers/a?b/json" {
		t.Fatalf("path = %q", got.path)
	}
}

// An engine that accepts connections but never answers /version (podman's
// service stuck on its storage lock) must not wedge every other call behind
// the client's mutex: each probe gives up after versionTimeout.
func TestAStuckVersionProbeBlocksNoOtherCall(t *testing.T) {
	old := versionTimeout
	versionTimeout = 200 * time.Millisecond
	t.Cleanup(func() { versionTimeout = old })

	dir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stuck := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			select {
			case <-stuck:
			case <-r.Context().Done():
			}
			return
		}
		io.WriteString(w, "[]")
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() { close(stuck); srv.Close() })

	c := New(sock)
	go c.prefix(context.Background()) // the events loop: no deadline of its own
	time.Sleep(50 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := c.ListContainers(ctx, "x")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListContainers: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListContainers waited on the stuck /version probe")
	}
}

// A name is one path segment: "../containers/x" must not walk to another
// endpoint of the engine.
func TestNamesAreEscapedInTheURLPath(t *testing.T) {
	uris := make(chan string, 4)
	dir, err := os.MkdirTemp("", "pm")
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
			io.WriteString(w, `{"MinAPIVersion":"1.24"}`)
			return
		}
		uris <- r.RequestURI
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	c := New(sock)
	c.RemoveVolume(ctx, "../containers/x")
	c.RemoveNetwork(ctx, "../containers/y")
	for _, want := range []string{"/v1.41/volumes/..%2Fcontainers%2Fx", "/v1.41/networks/..%2Fcontainers%2Fy"} {
		if got := <-uris; got != want {
			t.Errorf("request URI = %q, want %q", got, want)
		}
	}
}
