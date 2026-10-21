package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
)

// serviceKeys are the service keys anastasis understands. Anything else is
// rejected. depends_on and restart are accepted and ignored (depends_on
// must still name services of the project): anastasis retries until
// dependencies are up and owns restarts itself.
// container_name is ignored because anastasis names every replica itself.
var serviceKeys = []string{
	"name", "image", "command", "entrypoint", "environment", "env_file", "volumes", "healthcheck",
	"labels", "user", "working_dir", "stop_signal", "stop_grace_period", "cap_add", "cap_drop",
	"read_only", "deploy", "ports", "networks", "depends_on", "restart", "container_name", "x-anastasis",
}

func toService(project, name string, a at, dir string, vars, vols map[string]string) (model.Service, error) {
	fail := func(err error) (model.Service, error) { return model.Service{}, err }
	if _, err := a.mapping(); err != nil {
		return fail(err)
	}
	switch {
	case a.has("build"):
		return fail(errors.New("build: is not supported; push an image and use image:"))
	case a.has("network_mode"):
		return fail(errors.New("network_mode: is not supported; anastasis puts every service on one network per project"))
	}
	if err := a.only(serviceKeys...); err != nil {
		return fail(err)
	}
	if err := checkNetworks(a.key("networks")); err != nil {
		return fail(err)
	}
	r, err := a.key("restart").str()
	if err != nil {
		return fail(err)
	}
	if r == "no" || strings.HasPrefix(r, "on-failure") {
		return fail(fmt.Errorf("restart: %s is not supported; anastasis keeps every service running (run one-shot jobs with podman run)", r))
	}
	c, err := containerConfig(a, dir, vars, vols)
	if err != nil {
		return fail(err)
	}
	svc := model.Service{Project: project, Name: name, Replicas: 1, StopGrace: 10 * time.Second, Config: c}
	if g := a.key("stop_grace_period"); g.v != nil {
		d, err := g.duration()
		if err != nil {
			return fail(err)
		}
		svc.StopGrace = time.Duration(d)
	}
	if err := deploy(a.key("deploy"), &svc); err != nil {
		return fail(err)
	}
	if err := extension(a.key("x-anastasis"), &svc); err != nil {
		return fail(err)
	}
	if svc.Update.Parallelism < 1 {
		// 0 means all at once, as in Compose; an autoscaled service can grow to max.
		svc.Update.Parallelism = max(svc.Replicas, 1)
		if svc.Autoscale != nil {
			svc.Update.Parallelism = svc.Autoscale.Max
		}
	}
	svc.Hash = model.ConfigHash(svc)
	return svc, nil
}

// checkDependsOn rejects depends_on entries (a list or a mapping) that name
// no service in services.
func checkDependsOn(a at, services map[string]any) error {
	var names []string
	switch v := a.v.(type) {
	case nil:
	case []any:
		var err error
		if names, err = a.strs(); err != nil {
			return err
		}
	case map[string]any:
		names = slices.Sorted(maps.Keys(v))
	default:
		return a.errorf("want a list or a mapping, got %s", kind(a.v))
	}
	for _, n := range names {
		if _, ok := services[n]; !ok {
			return a.errorf("undefined service %q", n)
		}
	}
	return nil
}

func checkNetworks(a at) error {
	var names []string
	switch v := a.v.(type) {
	case nil:
	case []any:
		var err error
		if names, err = a.strs(); err != nil {
			return err
		}
	case map[string]any:
		names = slices.Collect(maps.Keys(v))
	default:
		return a.errorf("want a list or a mapping, got %s", kind(a.v))
	}
	for _, n := range names {
		if n != "default" {
			return errors.New("custom networks are not supported; anastasis puts every service on one network per project")
		}
	}
	return nil
}

func containerConfig(a at, dir string, vars, vols map[string]string) (model.Config, error) {
	var c model.Config
	var err error
	if c.Image, err = a.key("image").str(); err != nil {
		return c, err
	}
	if c.Image == "" {
		return c, errors.New("image: is required")
	}
	if c.Command, err = a.key("command").command(); err != nil {
		return c, err
	}
	if c.Entrypoint, err = a.key("entrypoint").command(); err != nil {
		return c, err
	}
	if c.Environment, err = environment(a, dir, vars); err != nil {
		return c, err
	}
	if c.Labels, err = labels(a.key("labels")); err != nil {
		return c, err
	}
	for k, dst := range map[string]*string{"user": &c.User, "working_dir": &c.WorkingDir, "stop_signal": &c.StopSignal} {
		if *dst, err = a.key(k).str(); err != nil {
			return c, err
		}
	}
	if c.CapAdd, err = a.key("cap_add").strs(); err != nil {
		return c, err
	}
	if c.CapDrop, err = a.key("cap_drop").strs(); err != nil {
		return c, err
	}
	if c.ReadOnly, err = a.key("read_only").boolean(); err != nil {
		return c, err
	}
	if c.HealthCheck, err = healthcheck(a.key("healthcheck")); err != nil {
		return c, err
	}
	if c.Ports, err = ports(a.key("ports")); err != nil {
		return c, err
	}
	if c.Volumes, err = serviceVolumes(a.key("volumes"), dir, vols); err != nil {
		return c, err
	}
	c.Deploy, err = limits(a.key("deploy").key("resources"))
	return c, err
}

// environment merges env_file files (in order) and then environment:, which
// wins. A bare KEY takes its value from the variables, or is left out.
func environment(a at, dir string, vars map[string]string) (map[string]string, error) {
	env := map[string]string{}
	files, err := envFiles(a.key("env_file"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		m, err := readEnvFile(resolvePath(f.path, dir), vars)
		if errors.Is(err, fs.ErrNotExist) && !f.required {
			continue
		}
		if err != nil {
			return nil, a.key("env_file").errorf("%v", err)
		}
		maps.Copy(env, m)
	}
	pairs, err := a.key("environment").pairs()
	if err != nil {
		return nil, err
	}
	for k, v := range pairs {
		if v != nil {
			env[k] = *v
		} else if val, ok := vars[k]; ok {
			env[k] = val
		}
	}
	if len(env) == 0 {
		return nil, nil
	}
	return env, nil
}

type envFile struct {
	path     string
	required bool
}

func envFiles(a at) ([]envFile, error) {
	switch v := a.v.(type) {
	case nil:
		return nil, nil
	case string:
		return []envFile{{v, true}}, nil
	case []any:
		var out []envFile
		for i, e := range v {
			item := a.index(i, e)
			if s, ok := e.(string); ok {
				out = append(out, envFile{s, true})
				continue
			}
			if err := item.only("path", "required"); err != nil {
				return nil, err
			}
			p, err := item.key("path").str()
			if err != nil {
				return nil, err
			}
			req := true
			if item.has("required") {
				if req, err = item.key("required").boolean(); err != nil {
					return nil, err
				}
			}
			out = append(out, envFile{p, req})
		}
		return out, nil
	}
	return nil, a.errorf("want a path or a list of paths, got %s", kind(a.v))
}

func labels(a at) (map[string]string, error) {
	pairs, err := a.pairs()
	if err != nil || len(pairs) == 0 {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range pairs {
		out[k] = ""
		if v != nil {
			out[k] = *v
		}
	}
	return out, nil
}

func healthcheck(a at) (*model.HealthCheck, error) {
	if a.v == nil {
		return nil, nil
	}
	if err := a.only("test", "interval", "timeout", "start_period", "start_interval", "retries", "disable"); err != nil {
		return nil, err
	}
	h := &model.HealthCheck{}
	var err error
	if s, ok := a.key("test").v.(string); ok {
		h.Test = []string{"CMD-SHELL", s}
	} else if h.Test, err = a.key("test").strs(); err != nil {
		return nil, err
	}
	for k, dst := range map[string]*model.Duration{"interval": &h.Interval, "timeout": &h.Timeout, "start_period": &h.StartPeriod} {
		if *dst, err = a.key(k).duration(); err != nil {
			return nil, err
		}
	}
	if h.Retries, err = a.key("retries").integer(0); err != nil {
		return nil, err
	}
	h.Disable, err = a.key("disable").boolean()
	return h, err
}

func ports(a at) ([]model.Port, error) {
	items, err := a.list()
	if err != nil {
		return nil, err
	}
	var out []model.Port
	for i, e := range items {
		item := a.index(i, e)
		var p model.Port
		if _, ok := e.(map[string]any); ok {
			p, err = longPort(item)
		} else {
			s, serr := item.str()
			if serr != nil {
				return nil, serr
			}
			if p, err = parsePort(s); err != nil {
				err = item.errorf("%v", err)
			}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func longPort(a at) (model.Port, error) {
	if err := a.only("target", "published", "host_ip", "protocol", "mode", "name", "app_protocol"); err != nil {
		return model.Port{}, err
	}
	t, err := a.key("target").integer(0)
	if err != nil {
		return model.Port{}, err
	}
	if t < 1 || t > 65535 {
		return model.Port{}, a.key("target").errorf("want a port 1-65535")
	}
	p := model.Port{Target: t, Protocol: "tcp"}
	if p.Published, err = a.key("published").str(); err != nil {
		return p, err
	}
	if strings.Contains(p.Published, "-") {
		return p, a.key("published").errorf("port ranges are not supported")
	}
	if p.HostIP, err = a.key("host_ip").str(); err != nil {
		return p, err
	}
	if proto, err := a.key("protocol").str(); err != nil {
		return p, err
	} else if proto != "" {
		if p.Protocol, err = protocol(proto); err != nil {
			return p, a.key("protocol").errorf("%v", err)
		}
	}
	return p, nil
}

func serviceVolumes(a at, dir string, vols map[string]string) ([]model.Volume, error) {
	items, err := a.list()
	if err != nil {
		return nil, err
	}
	var out []model.Volume
	for i, e := range items {
		item := a.index(i, e)
		var v model.Volume
		if _, ok := e.(map[string]any); ok {
			v, err = longVolume(item, dir)
		} else {
			s, serr := item.str()
			if serr != nil {
				return nil, serr
			}
			if v, err = parseVolume(s, dir); err != nil {
				err = item.errorf("%v", err)
			}
		}
		if err != nil {
			return nil, err
		}
		if v.Type == model.VolumeVolume && v.Source != "" {
			full, ok := vols[v.Source]
			if !ok {
				return nil, item.errorf("refers to undefined volume %s; declare it under the top-level volumes:", v.Source)
			}
			v.Source = full
		}
		out = append(out, v)
	}
	return out, nil
}

func longVolume(a at, dir string) (model.Volume, error) {
	var v model.Volume
	if err := a.only("type", "source", "target", "read_only", "bind", "volume", "tmpfs", "consistency"); err != nil {
		return v, err
	}
	if a.has("tmpfs") {
		return v, a.key("tmpfs").errorf("tmpfs options are not supported")
	}
	var err error
	if v.Type, err = a.key("type").str(); err != nil {
		return v, err
	}
	src, err := a.key("source").str()
	if err != nil {
		return v, err
	}
	target, err := a.key("target").str()
	if err != nil {
		return v, err
	}
	if !strings.HasPrefix(target, "/") {
		return v, a.key("target").errorf("want an absolute path in the container")
	}
	v.Target = path.Clean(target)
	if v.ReadOnly, err = a.key("read_only").boolean(); err != nil {
		return v, err
	}
	if a.has("bind") && v.Type != model.VolumeBind {
		return v, a.key("bind").errorf("only applies to type: bind")
	}
	switch v.Type {
	case model.VolumeBind:
		if src == "" {
			return v, a.key("source").errorf("is required for a bind mount")
		}
		v.Source = resolvePath(src, dir)
		if b := a.key("bind"); b.v != nil {
			if err := b.only("selinux", "create_host_path", "propagation", "recursive"); err != nil {
				return v, err
			}
			if b.has("propagation") || b.has("recursive") {
				return v, b.errorf("bind propagation and recursive are not supported")
			}
			sel, err := b.key("selinux").str()
			if err != nil {
				return v, err
			}
			if sel != "" && sel != "z" && sel != "Z" {
				return v, b.key("selinux").errorf("want z or Z, got %q", sel)
			}
			v.Bind = &model.Bind{SELinux: sel}
			if b.has("create_host_path") {
				create, err := b.key("create_host_path").boolean()
				if err != nil {
					return v, err
				}
				if !create {
					v.Bind.CreateHostPath = &create
				}
			}
		}
	case model.VolumeVolume:
		v.Source = src
		if o := a.key("volume"); o.v != nil {
			if err := o.only("nocopy", "subpath"); err != nil {
				return v, err
			}
			nocopy, err := o.key("nocopy").boolean()
			if err != nil {
				return v, err
			}
			sub, err := o.key("subpath").str()
			if err != nil {
				return v, err
			}
			if nocopy || sub != "" {
				return v, o.errorf("nocopy and subpath are not supported")
			}
		}
	case model.VolumeTmpfs:
	default:
		return v, a.key("type").errorf("%q is not supported (bind, volume or tmpfs)", v.Type)
	}
	return v, nil
}

// limits reads deploy.resources. Reservations are accepted and ignored:
// they only guide schedulers, and anastasis sizes replicas by their limits.
func limits(a at) (*model.Deploy, error) {
	if a.v == nil {
		return nil, nil
	}
	if err := a.only("limits", "reservations"); err != nil {
		return nil, err
	}
	l := a.key("limits")
	if l.v == nil {
		return nil, nil
	}
	if err := l.only("cpus", "memory"); err != nil {
		return nil, err
	}
	cpus, err := l.key("cpus").number()
	if err != nil {
		return nil, err
	}
	mem, err := l.key("memory").size()
	if err != nil {
		return nil, err
	}
	return &model.Deploy{Resources: model.Resources{Limits: &model.Limits{CPUs: cpus, Memory: model.Bytes(mem)}}}, nil
}

func deploy(a at, svc *model.Service) error {
	hasPorts := len(svc.Config.Ports) > 0
	u := model.UpdateConfig{Parallelism: 1, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "pause"}
	if hasPorts {
		u.Order = "stop-first"
	}
	if a.v != nil {
		if err := a.only("mode", "replicas", "resources", "update_config", "endpoint_mode", "placement", "rollback_config", "restart_policy", "labels"); err != nil {
			return err
		}
		mode, err := a.key("mode").str()
		if err != nil {
			return err
		}
		pl, err := a.key("placement").mapping()
		if err != nil {
			return err
		}
		switch {
		case mode != "" && mode != "replicated":
			return fmt.Errorf("deploy.mode: %s is not supported", mode)
		case a.has("endpoint_mode"):
			return errors.New("deploy.endpoint_mode is not supported")
		case len(pl) > 0:
			return errors.New("deploy.placement is not supported")
		case a.has("rollback_config"):
			return errors.New("deploy.rollback_config is not supported; use update_config.failure_action: rollback")
		}
		cond, err := a.key("restart_policy").key("condition").str()
		if err != nil {
			return err
		}
		if cond == "none" || cond == "on-failure" {
			return fmt.Errorf("deploy.restart_policy.condition: %s is not supported; anastasis keeps every service running", cond)
		}
		if svc.Replicas, err = a.key("replicas").integer(1); err != nil {
			return err
		}
		if uc := a.key("update_config"); uc.v != nil {
			if err := updateConfig(uc, &u, hasPorts); err != nil {
				return err
			}
		}
	}
	if svc.Replicas < 0 {
		return errors.New("deploy.replicas must be >= 0")
	}
	if hasPorts && svc.Replicas > 1 {
		return errors.New("ports: cannot be used with replicas > 1; move it to x-anastasis.expose")
	}
	svc.Update = u
	return nil
}

func updateConfig(a at, u *model.UpdateConfig, hasPorts bool) error {
	if err := a.only("parallelism", "delay", "monitor", "order", "failure_action", "max_failure_ratio"); err != nil {
		return err
	}
	ratio, err := a.key("max_failure_ratio").number()
	if err != nil {
		return err
	}
	if ratio != 0 {
		return errors.New("deploy.update_config.max_failure_ratio is not supported")
	}
	if a.has("parallelism") {
		if u.Parallelism, err = a.key("parallelism").integer(0); err != nil {
			return err
		}
	}
	d, err := a.key("delay").duration()
	if err != nil {
		return err
	}
	u.Delay = time.Duration(d)
	if m, err := a.key("monitor").duration(); err != nil {
		return err
	} else if m != 0 {
		u.Monitor = time.Duration(m)
	}
	order, err := a.key("order").str()
	if err != nil {
		return err
	}
	if order != "" {
		if order != "start-first" && order != "stop-first" {
			return errors.New("deploy.update_config.order must be start-first or stop-first")
		}
		if hasPorts && order == "start-first" {
			return errors.New("ports: cannot be used with order start-first (the new replica cannot bind the port); use x-anastasis.expose")
		}
		u.Order = order
	}
	fa, err := a.key("failure_action").str()
	if err != nil {
		return err
	}
	if fa != "" {
		if fa != "pause" && fa != "rollback" && fa != "continue" {
			return errors.New("deploy.update_config.failure_action must be pause, rollback or continue")
		}
		u.FailureAction = fa
	}
	return nil
}

// extension reads the service-level x-anastasis block.
func extension(a at, svc *model.Service) error {
	if a.v == nil {
		return nil
	}
	if err := a.only("expose", "sticky", "autoscale"); err != nil {
		return err
	}
	exp, err := a.key("expose").str()
	if err != nil {
		return err
	}
	if exp != "" {
		e, err := parseExpose(exp)
		if err != nil {
			return fmt.Errorf("x-anastasis.expose: %w", err)
		}
		svc.Expose = &e
	}
	if svc.Sticky, err = a.key("sticky").boolean(); err != nil {
		return err
	}
	if svc.Sticky && svc.Expose == nil {
		return errors.New("x-anastasis.sticky needs x-anastasis.expose")
	}
	if as := a.key("autoscale"); as.v != nil {
		if err := as.only("min", "max", "inflight", "cpu"); err != nil {
			return err
		}
		auto, err := toAutoscale(as, *svc)
		if err != nil {
			return fmt.Errorf("x-anastasis.autoscale: %w", err)
		}
		svc.Autoscale = &auto
		svc.Replicas = min(max(svc.Replicas, auto.Min), auto.Max)
	}
	return nil
}

// toAutoscale validates x-anastasis.autoscale. min defaults to deploy.replicas.
func toAutoscale(a at, svc model.Service) (model.Autoscale, error) {
	as := model.Autoscale{Min: svc.Replicas}
	var err error
	if as.Max, err = a.key("max").integer(0); err != nil {
		return as, err
	}
	if a.has("min") {
		if as.Min, err = a.key("min").integer(0); err != nil {
			return as, err
		}
	}
	if as.Inflight, err = a.key("inflight").number(); err != nil {
		return as, err
	}
	if as.CPU, err = a.key("cpu").number(); err != nil {
		return as, err
	}
	lim := svc.Config.Deploy
	switch {
	case as.Max < 1:
		return as, errors.New("max is required and must be >= 1")
	case as.Min < 1:
		return as, errors.New("min must be >= 1 (defaults to deploy.replicas; scaling to zero is not supported)")
	case as.Min > as.Max:
		return as, fmt.Errorf("min %d (defaults to deploy.replicas) is greater than max %d", as.Min, as.Max)
	case as.Inflight < 0 || as.CPU < 0:
		return as, errors.New("inflight and cpu must be positive")
	case as.Inflight == 0 && as.CPU == 0:
		return as, errors.New("set inflight, cpu or both")
	case len(svc.Config.Ports) > 0:
		return as, errors.New("a service with ports: runs one replica; use x-anastasis.expose to autoscale")
	case as.Inflight > 0 && svc.Expose == nil:
		return as, errors.New("inflight needs x-anastasis.expose (only the load balancer counts requests)")
	case as.CPU > 0 && (lim == nil || lim.Resources.Limits == nil || lim.Resources.Limits.CPUs == 0):
		return as, errors.New("cpu needs deploy.resources.limits.cpus (a percentage of no limit means nothing)")
	}
	if as.CPU > 0 {
		as.CPULimit = lim.Resources.Limits.CPUs
	}
	return as, nil
}
