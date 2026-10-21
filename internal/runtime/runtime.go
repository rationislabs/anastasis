// Package runtime talks to Podman (or Docker) through the Docker-compatible
// API. It is the only package that touches containers.
package runtime

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
	"github.com/rationislabs/anastasis/internal/podman"
)

// Every API call gets a deadline, so a hung Podman or a stalled registry
// cannot block the reconcile loop forever.
const (
	apiTimeout  = 30 * time.Second
	pullTimeout = 10 * time.Minute
)

// Labels anastasis puts on every container it creates.
const (
	LabelProject = "anastasis.project"
	LabelService = "anastasis.service"
	LabelHash    = "anastasis.hash"
)

// Runtime is a client for one Podman or Docker API socket.
type Runtime struct {
	c *podman.Client
}

// New returns a client for the API socket at socket. It does not connect
// until the first call.
func New(socket string) (*Runtime, error) {
	return &Runtime{c: podman.New(socket)}, nil
}

// NetworkName is the network every replica of a project joins.
func NetworkName(project string) string { return "anastasis-" + project }

// List returns every anastasis container, keyed by project.
func (r *Runtime) List(ctx context.Context) (map[string][]model.Replica, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	ids, err := r.c.ListContainers(ctx, LabelProject)
	if err != nil {
		return nil, err
	}
	out := map[string][]model.Replica{}
	for _, id := range ids {
		in, err := r.c.InspectContainer(ctx, id)
		if errors.Is(err, podman.ErrNotFound) {
			continue // removed since the list call
		}
		if err != nil {
			return nil, err
		}
		rep := model.Replica{
			ID:      in.ID,
			Name:    strings.TrimPrefix(in.Name, "/"),
			Service: in.Config.Labels[LabelService],
			Hash:    in.Config.Labels[LabelHash],
			State:   in.State.Status,
		}
		rep.Health = health(in, time.Now())
		rep.Started, _ = time.Parse(time.RFC3339Nano, in.State.StartedAt)
		rep.Finished, _ = time.Parse(time.RFC3339Nano, in.State.FinishedAt)
		for _, bindings := range in.NetworkSettings.Ports {
			for _, b := range bindings {
				if b.HostIP == "127.0.0.1" {
					rep.HostPort, _ = strconv.Atoi(b.HostPort)
				}
			}
		}
		p := in.Config.Labels[LabelProject]
		out[p] = append(out[p], rep)
	}
	return out, nil
}

// health is the replica's health status, or "stalled" when its healthcheck
// is overdue. Podman 4.9 never times out a hung check, so the status stays
// at its last value: a replica that hangs would keep getting traffic.
// Overdue past interval+timeout, the replica leaves the load balancer;
// overdue past retries of those, it is reported unhealthy and replaced.
// A check that never ran (no systemd user session) keeps "starting".
func health(in podman.Container, now time.Time) string {
	h := in.State.Health
	if h == nil {
		return ""
	}
	if h.Status == "unhealthy" || len(h.Log) == 0 {
		return h.Status
	}
	last, err := time.Parse(time.RFC3339Nano, h.Log[len(h.Log)-1].Start)
	if err != nil {
		return h.Status
	}
	interval, timeout, retries := 30*time.Second, 30*time.Second, 3 // engine defaults
	if hc := in.Config.Healthcheck; hc != nil {
		interval = cmp.Or(hc.Interval, interval)
		timeout = cmp.Or(hc.Timeout, timeout)
		retries = cmp.Or(hc.Retries, retries)
	}
	const slack = 5 * time.Second // timer jitter
	period, overdue := interval+timeout, now.Sub(last)
	switch {
	case overdue > time.Duration(retries)*period+slack:
		return "unhealthy"
	case overdue > period+slack:
		return "stalled"
	}
	return h.Status
}

// Create creates and starts one replica of svc labelled with hash and
// returns its name.
func (r *Runtime) Create(ctx context.Context, svc model.Service, hash string) (string, error) {
	if err := ensureBindSources(svc); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	name := fmt.Sprintf("%s-%s-%s", svc.Project, svc.Name, randomHex(2))
	id, err := r.c.CreateContainer(ctx, name, containerSpec(svc, hash))
	if err != nil {
		return "", err
	}
	if err := r.c.StartContainer(ctx, id); err != nil {
		_ = r.c.RemoveContainer(ctx, id)
		return "", err
	}
	return name, nil
}

// Remove stops a container, giving it grace to exit, and deletes it with
// its anonymous volumes.
func (r *Runtime) Remove(ctx context.Context, id string, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, grace+apiTimeout)
	defer cancel()
	if err := r.c.StopContainer(ctx, id, grace); err != nil && !errors.Is(err, podman.ErrNotFound) {
		return err
	}
	if err := r.c.RemoveContainer(ctx, id); err != nil && !errors.Is(err, podman.ErrNotFound) {
		return err
	}
	return nil
}

// Pull makes sure ref is present. Without force it skips images that are
// already there.
func (r *Runtime) Pull(ctx context.Context, ref string, force bool) error {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	if !force {
		if ok, err := r.c.ImageExists(ctx, ref); err == nil && ok {
			return nil
		}
	}
	return r.c.Pull(ctx, ref)
}

// EnsureNetwork creates the project's network if it does not exist.
func (r *Runtime) EnsureNetwork(ctx context.Context, project string) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	name := NetworkName(project)
	ok, err := r.c.NetworkExists(ctx, name)
	if err != nil || ok {
		return err
	}
	return r.c.CreateNetwork(ctx, name, map[string]string{LabelProject: project})
}

// RemoveNetwork deletes the project's network if it exists.
func (r *Runtime) RemoveNetwork(ctx context.Context, project string) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if err := r.c.RemoveNetwork(ctx, NetworkName(project)); err != nil && !errors.Is(err, podman.ErrNotFound) {
		return err
	}
	return nil
}

// RemoveVolume deletes a named volume if it exists.
func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if err := r.c.RemoveVolume(ctx, name); err != nil && !errors.Is(err, podman.ErrNotFound) {
		return err
	}
	return nil
}

// Logs returns the last n lines a container wrote, for diagnosing crashes.
func (r *Runtime) Logs(ctx context.Context, id string, n int) string {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	s, err := r.c.Logs(ctx, id, n)
	if err != nil {
		return "logs unavailable: " + err.Error()
	}
	return strings.TrimSpace(s)
}

// Stats returns a container's cumulative CPU time and when the runtime
// read it. Rootless Podman on cgroup v1 cannot report it; that comes back
// as an error.
func (r *Runtime) Stats(ctx context.Context, id string) (time.Duration, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	u, err := r.c.Usage(ctx, id)
	if err != nil {
		return 0, time.Time{}, err
	}
	if u.CPU == 0 {
		return 0, time.Time{}, errors.New("runtime reports no CPU usage (rootless Podman needs cgroup v2)")
	}
	return time.Duration(u.CPU), u.Read, nil
}

// eventFilters asks for container events only. Podman 4.9 matches no event
// at all on a label filter without a value, so callers keep anastasis's own
// events with Ours.
var eventFilters = map[string][]string{"type": {"container"}}

// Events streams container events; Ours tells anastasis's apart.
func (r *Runtime) Events(ctx context.Context) (<-chan podman.Event, <-chan error) {
	return r.c.Events(ctx, eventFilters)
}

// Ours reports whether e is about an anastasis container.
func Ours(e podman.Event) bool {
	_, ok := e.Actor.Attributes[LabelProject]
	return ok
}

// Wakes reports whether e can change what a reconcile pass sees: a
// container started, died, went away or changed health. Healthchecks emit
// exec events (Docker) and a health_status per run (Podman); those cannot.
// last holds each container's latest health event; Wakes keeps it current.
func Wakes(e podman.Event, last map[string]string) bool {
	switch e.Action {
	case "start", "die", "oom":
		return true
	case "destroy", "remove": // Podman says remove
		delete(last, e.Actor.ID)
		return true
	}
	if !strings.HasPrefix(e.Action, "health_status") {
		return false
	}
	h := e.Action + e.HealthStatus
	if last[e.Actor.ID] == h {
		return false
	}
	last[e.Actor.ID] = h
	return true
}

// containerSpec turns a service into the API's create body.
func containerSpec(svc model.Service, hash string) podman.CreateSpec {
	c := svc.Config
	labels := map[string]string{}
	for k, v := range c.Labels {
		labels[k] = v
	}
	labels[LabelProject], labels[LabelService], labels[LabelHash] = svc.Project, svc.Name, hash

	var env []string
	for k, v := range c.Environment {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)

	s := podman.CreateSpec{
		Image:      c.Image,
		Cmd:        c.Command,
		Entrypoint: c.Entrypoint,
		Env:        env,
		Labels:     labels,
		User:       c.User,
		WorkingDir: c.WorkingDir,
		StopSignal: c.StopSignal,
		HostConfig: podman.HostConfig{
			NetworkMode:    NetworkName(svc.Project),
			CapAdd:         c.CapAdd,
			CapDrop:        c.CapDrop,
			ReadonlyRootfs: c.ReadOnly,
		},
		NetworkingConfig: podman.NetworkingConfig{EndpointsConfig: map[string]podman.EndpointSettings{
			NetworkName(svc.Project): {Aliases: []string{svc.Name}},
		}},
	}
	if hc := c.HealthCheck; hc != nil && hc.Disable {
		s.Healthcheck = &podman.Healthcheck{Test: []string{"NONE"}} // also turns off the image's HEALTHCHECK
	} else if hc != nil {
		s.Healthcheck = &podman.Healthcheck{
			Test:        hc.Test,
			Interval:    time.Duration(hc.Interval),
			Timeout:     time.Duration(hc.Timeout),
			StartPeriod: time.Duration(hc.StartPeriod),
			Retries:     hc.Retries,
		}
	}
	h := &s.HostConfig
	if d := c.Deploy; d != nil && d.Resources.Limits != nil {
		h.NanoCPUs = int64(d.Resources.Limits.CPUs * 1e9)
		h.Memory = int64(d.Resources.Limits.Memory)
	}
	for _, v := range c.Volumes {
		if v.Bind != nil && v.Bind.SELinux != "" {
			// The mounts API has no SELinux relabel option; the bind string does.
			opts := v.Bind.SELinux
			if v.ReadOnly {
				opts = "ro," + opts
			}
			h.Binds = append(h.Binds, v.Source+":"+v.Target+":"+opts)
			continue
		}
		h.Mounts = append(h.Mounts, podman.Mount{Type: v.Type, Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
	}

	ports, bindings := map[string]struct{}{}, map[string][]podman.PortBinding{}
	if e := svc.Expose; e != nil {
		// Rootless container IPs are not reachable from the host, so the
		// load balancer reaches each replica through a random loopback port.
		p := fmt.Sprintf("%d/tcp", e.ContainerPort)
		ports[p] = struct{}{}
		bindings[p] = append(bindings[p], podman.PortBinding{HostIP: "127.0.0.1"})
	}
	for _, sp := range c.Ports {
		proto := sp.Protocol
		if proto == "" {
			proto = "tcp"
		}
		p := fmt.Sprintf("%d/%s", sp.Target, proto)
		ports[p] = struct{}{}
		bindings[p] = append(bindings[p], podman.PortBinding{HostIP: sp.HostIP, HostPort: sp.Published})
	}
	if len(ports) > 0 {
		s.ExposedPorts, h.PortBindings = ports, bindings
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ensureBindSources creates missing bind-mount host directories, as Compose
// does; the mounts API would refuse them instead.
func ensureBindSources(svc model.Service) error {
	for _, v := range svc.Config.Volumes {
		if v.CreatesSource() {
			if err := os.MkdirAll(v.Source, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}
