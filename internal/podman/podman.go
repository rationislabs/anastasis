// Package podman is a client for the Docker-compatible REST API that Podman
// serves on its socket (Docker serves the same API, which is how tests run
// on machines without Podman). It uses only the standard library and
// covers only the calls anastasis makes.
package podman

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// baseVersion is the API version this client is written against. Podman 4+
// and Docker up to 28 serve it; Docker 29 and later refuse anything below
// their minimum (1.44), so the client moves up to the engine's minimum when
// that is higher.
const baseVersion = "1.41"

// ErrNotFound matches (with errors.Is) every 404 answer.
var ErrNotFound = errors.New("not found")

// APIError is an answer outside 2xx and 304.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// Client talks to one API socket. Callers set deadlines through ctx.
type Client struct {
	hc      *http.Client
	mu      sync.Mutex
	version string // set once the engine answered GET /version
}

// prefix returns the versioned path prefix, asking the engine for its
// minimum API version until it has answered once.
func (c *Client) prefix(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != "" {
		return "/v" + c.version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://podman/version", nil)
	if err != nil {
		return "/v" + baseVersion
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "/v" + baseVersion // engine down: try again on the next call
	}
	defer resp.Body.Close()
	var v struct {
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	c.version = baseVersion
	if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&v) == nil && newer(v.MinAPIVersion, baseVersion) {
		c.version = v.MinAPIVersion
	}
	return "/v" + c.version
}

// newer reports whether API version a ("1.44") is above b.
func newer(a, b string) bool {
	am, an, ok1 := splitVersion(a)
	bm, bn, ok2 := splitVersion(b)
	return ok1 && ok2 && (am > bm || am == bm && an > bn)
}

func splitVersion(v string) (int, int, bool) {
	maj, mnr, ok := strings.Cut(v, ".")
	a, err1 := strconv.Atoi(maj)
	b, err2 := strconv.Atoi(mnr)
	return a, b, ok && err1 == nil && err2 == nil
}

func New(socket string) *Client {
	return &Client{hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// do sends a request and returns the response for 2xx and 304 answers;
// the caller closes the body.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := "http://podman" + c.prefix(ctx) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) != nil || m.Message == "" {
		m.Message = strings.TrimSpace(string(b))
	}
	return nil, &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: m.Message}
}

// call sends a request and decodes the answer into out (nil: discard it).
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusNoContent {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// exists turns a 404 into false.
func (c *Client) exists(ctx context.Context, path string) (bool, error) {
	err := c.call(ctx, http.MethodGet, path, nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func filtersQuery(f map[string][]string) string {
	b, _ := json.Marshal(f) // map of string slices, always marshals
	return string(b)
}

// ListContainers returns the ids of all containers (any state) that carry
// label.
func (c *Client) ListContainers(ctx context.Context, label string) ([]string, error) {
	var out []struct {
		ID string `json:"Id"`
	}
	q := url.Values{"all": {"true"}, "filters": {filtersQuery(map[string][]string{"label": {label}})}}
	if err := c.call(ctx, http.MethodGet, "/containers/json", q, nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out))
	for i, s := range out {
		ids[i] = s.ID
	}
	return ids, nil
}

// Container is the part of an inspect answer anastasis reads.
type Container struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Labels      map[string]string
		Healthcheck *Healthcheck
	}
	State struct {
		Status     string
		StartedAt  string
		FinishedAt string
		Health     *Health
	}
	NetworkSettings struct {
		Ports map[string][]PortBinding
	}
}

type Health struct {
	Status string
	Log    []HealthLog // oldest first
}

// HealthLog is one healthcheck run. Times are RFC 3339.
type HealthLog struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	ExitCode int    `json:"exitCode"`
	Output   string `json:"output"`
}

type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string
}

func (c *Client) InspectContainer(ctx context.Context, id string) (Container, error) {
	var out Container
	err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
	return out, err
}

// CreateSpec is the body of a container create call.
type CreateSpec struct {
	Image            string
	Cmd              []string            // null keeps the image's CMD, [] clears it
	Entrypoint       []string            // null keeps the image's ENTRYPOINT, [] clears it
	Env              []string            `json:",omitempty"`
	Labels           map[string]string   `json:",omitempty"`
	User             string              `json:",omitempty"`
	WorkingDir       string              `json:",omitempty"`
	StopSignal       string              `json:",omitempty"`
	Healthcheck      *Healthcheck        `json:",omitempty"`
	ExposedPorts     map[string]struct{} `json:",omitempty"`
	HostConfig       HostConfig
	NetworkingConfig NetworkingConfig
}

// Healthcheck durations are nanoseconds on the wire, as time.Duration
// marshals.
type Healthcheck struct {
	Test        []string
	Interval    time.Duration `json:",omitempty"`
	Timeout     time.Duration `json:",omitempty"`
	StartPeriod time.Duration `json:",omitempty"`
	Retries     int           `json:",omitempty"`
}

type HostConfig struct {
	NetworkMode    string
	CapAdd         []string                 `json:",omitempty"`
	CapDrop        []string                 `json:",omitempty"`
	ReadonlyRootfs bool                     `json:",omitempty"`
	NanoCPUs       int64                    `json:"NanoCpus,omitempty"`
	Memory         int64                    `json:",omitempty"`
	Binds          []string                 `json:",omitempty"`
	Mounts         []Mount                  `json:",omitempty"`
	PortBindings   map[string][]PortBinding `json:",omitempty"`
}

type Mount struct {
	Type     string
	Source   string `json:",omitempty"`
	Target   string
	ReadOnly bool `json:",omitempty"`
}

type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointSettings
}

type EndpointSettings struct {
	Aliases []string
}

// CreateContainer creates (but does not start) a container and returns its id.
func (c *Client) CreateContainer(ctx context.Context, name string, spec CreateSpec) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, spec, &out)
	return out.ID, err
}

// StartContainer starts a container; one already running is not an error.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// StopContainer stops a container, giving it grace before it is killed; one
// already stopped is not an error. The API takes whole seconds, so grace
// rounds up: 500ms must not become an immediate kill.
func (c *Client) StopContainer(ctx context.Context, id string, grace time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int((grace + time.Second - 1) / time.Second))}}
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil)
}

// RemoveContainer deletes a container, running or not, with its anonymous
// volumes.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	q := url.Values{"force": {"true"}, "v": {"true"}}
	return c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil)
}

// Logs returns the last tail lines a container wrote to stdout and stderr.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}, "tail": {strconv.Itoa(tail)}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return demux(b), nil
}

// demux turns a multiplexed log stream into text. Each frame is an 8-byte
// header (stream byte, three zero bytes, big-endian payload size) and the
// payload. A container with a TTY sends plain text, returned as is.
func demux(b []byte) string {
	var out bytes.Buffer
	for len(b) >= 8 && b[0] <= 2 && b[1] == 0 && b[2] == 0 && b[3] == 0 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if 8+n > len(b) {
			break
		}
		out.Write(b[8 : 8+n])
		b = b[8+n:]
	}
	out.Write(b)
	return out.String()
}

// Usage is one sample of a container's resource use.
type Usage struct {
	CPU      uint64    `json:"cpu"`           // cumulative CPU time in nanoseconds
	Memory   uint64    `json:"memory"`        // bytes in use
	MemLimit uint64    `json:"memLimit"`      // bytes; the host's memory when unlimited
	Read     time.Time `json:"read,omitzero"` // when the runtime took the sample
}

// Usage samples a container's CPU time and memory.
func (c *Client) Usage(ctx context.Context, id string) (Usage, error) {
	var s struct {
		Read     time.Time `json:"read"`
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Limit uint64 `json:"limit"`
		} `json:"memory_stats"`
	}
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats", q, nil, &s)
	return Usage{CPU: s.CPUStats.CPUUsage.TotalUsage, Memory: s.MemoryStats.Usage, MemLimit: s.MemoryStats.Limit, Read: s.Read}, err
}

func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	return c.exists(ctx, "/images/"+ref+"/json")
}

// Pull downloads ref. Errors can arrive inside the 200 answer's progress
// stream, so the stream is read to the end.
func (c *Client) Pull(ctx context.Context, ref string) error {
	name, tag := splitRef(ref)
	resp, err := c.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		err := dec.Decode(&msg)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
	}
}

// splitRef splits an image reference into the pull call's fromImage and tag.
// A digest travels in tag. No tag means latest: an empty tag would pull
// every tag of the repository.
func splitRef(ref string) (name, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		name, tag = ref[:i], ref[i+1:]
		if j := strings.LastIndex(name, ":"); j > strings.LastIndex(name, "/") {
			name = name[:j]
		}
		return name, tag
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	return c.exists(ctx, "/networks/"+name)
}

func (c *Client) CreateNetwork(ctx context.Context, name string, labels map[string]string) error {
	body := struct {
		Name   string
		Labels map[string]string
	}{name, labels}
	return c.call(ctx, http.MethodPost, "/networks/create", nil, body, nil)
}

func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/networks/"+name, nil, nil, nil)
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/volumes/"+name, nil, nil, nil)
}

// Event is one engine event; anastasis only needs to know one happened.
type Event struct {
	Type         string
	Action       string
	HealthStatus string // Podman's health_status result; Docker puts it in Action
	Actor        struct {
		ID         string
		Attributes map[string]string
	}
}

// Events streams events matching filters until ctx ends or the stream
// breaks; the error channel then gets exactly one error.
func (c *Client) Events(ctx context.Context, filters map[string][]string) (<-chan Event, <-chan error) {
	msgs, errs := make(chan Event), make(chan error, 1)
	go func() {
		resp, err := c.do(ctx, http.MethodGet, "/events", url.Values{"filters": {filtersQuery(filters)}}, nil)
		if err != nil {
			errs <- err
			return
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			var e Event
			if err := dec.Decode(&e); err != nil {
				if errors.Is(err, io.EOF) {
					err = errors.New("event stream closed")
				}
				errs <- err
				return
			}
			select {
			case msgs <- e:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
	}()
	return msgs, errs
}
