// Package reconcile runs anastasis's control loop: it keeps every registered
// service's replicas, load balancer and rollouts in line with state.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rationislabs/anastasis/internal/autoscale"
	"github.com/rationislabs/anastasis/internal/config"
	"github.com/rationislabs/anastasis/internal/lb"
	"github.com/rationislabs/anastasis/internal/model"
	"github.com/rationislabs/anastasis/internal/plan"
	"github.com/rationislabs/anastasis/internal/runtime"
	"github.com/rationislabs/anastasis/internal/state"
)

const (
	tick          = 10 * time.Second
	defaultGrace  = 10 * time.Second
	slowHealthAge = 2 * time.Minute
)

// ServiceStatus is what `anastasis status` shows for one service.
type ServiceStatus struct {
	Project      string    `json:"project"`
	Service      string    `json:"service"`
	Desired      int       `json:"desired"`
	Ready        int       `json:"ready"`          // up-to-date and ready
	Outdated     int       `json:"outdated"`       // running an old config
	Serving      int       `json:"serving"`        // ready replicas of any config, getting traffic
	Auto         string    `json:"auto,omitempty"` // e.g. "2→4 (inflight 14.0/10, cpu 55/70)"
	Port         int       `json:"port,omitempty"`
	Paused       bool      `json:"paused,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	BackoffUntil time.Time `json:"backoffUntil,omitzero"`
	Error        string    `json:"error,omitempty"`
	RolledBack   string    `json:"rolledBack,omitempty"` // set until the next up
	Warnings     []string  `json:"warnings,omitempty"`
}

// Converged reports whether the service runs exactly what was asked for.
func (s ServiceStatus) Converged() bool {
	return s.Ready == s.Desired && s.Outdated == 0 && !s.Paused && s.Error == "" && s.RolledBack == ""
}

// Status is the result of the latest reconcile pass.
type Status struct {
	RuntimeError string          `json:"runtimeError,omitempty"`
	Services     []ServiceStatus `json:"services"`
	Warnings     []string        `json:"warnings,omitempty"`
}

// Controller owns the reconcile loop. Every change to desired state runs
// on the loop goroutine, so passes and API calls never race.
// ponytail: one goroutine for every project; a pass blocks while it drains a replica (up to stop_grace_period). Use one loop per project if many projects make passes slow.
type Controller struct {
	store *state.Store
	rt    *runtime.Runtime
	lb    *lb.LB
	auto  *autoscale.Sampler
	poke  chan struct{}
	ops   chan func(context.Context)

	started time.Time // replicas that died before this died unwatched

	mem map[string]*memory // per "project/service"; loop goroutine only

	mu     sync.Mutex // guards status
	status Status
}

// memory is per-service state that lives only in RAM.
type memory struct {
	hash       string
	backoff    plan.Backoff
	rollout    plan.Rollout
	pulled     string // hash whose image is known to be present
	forcePull  bool
	lb         *lb.Service
	seenReady  map[string]bool // replica IDs that were ever ready
	rolledBack string          // why the last rollout was rolled back; cleared by up
	seen       snapshot        // what the last pass saw, to record what changed
}

// New returns a controller; call Run to start it.
func New(store *state.Store, rt *runtime.Runtime, balancer *lb.LB) *Controller {
	return &Controller{
		store: store, rt: rt, lb: balancer,
		auto: autoscale.NewSampler(rt, autoscale.DefaultWindows),
		poke: make(chan struct{}, 1),
		ops:  make(chan func(context.Context)),
		mem:  map[string]*memory{},
	}
}

// ShortenAutoscaleWindowsForTest replaces the autoscaler's timing windows.
// Integration tests use it so scale-down does not take 5 real minutes;
// call it before Run.
func (c *Controller) ShortenAutoscaleWindowsForTest(w autoscale.Windows) {
	c.auto = autoscale.NewSampler(c.rt, w)
}

// Poke asks for a reconcile pass soon. Pokes coalesce.
func (c *Controller) Poke() {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx ends. Containers keep running after it returns.
func (c *Controller) Run(ctx context.Context) {
	c.started = time.Now()
	go c.watchEvents(ctx)
	go c.auto.Run(ctx, c.Poke)
	t := time.NewTicker(tick)
	defer t.Stop()
	c.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-c.ops:
			op(ctx) // runs its own pass
		case <-c.poke:
			c.pass(ctx)
		case <-t.C:
			c.pass(ctx)
		}
	}
}

// do runs f on the loop goroutine, followed by a pass when f succeeds, so
// Status already reflects the change when do returns.
func (c *Controller) do(ctx context.Context, f func(context.Context) error) error {
	done := make(chan error, 1)
	op := func(lctx context.Context) {
		err := f(lctx)
		if err == nil {
			c.pass(lctx)
		}
		done <- err
	}
	select {
	case c.ops <- op:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Up loads a Compose file and registers or updates its project.
func (c *Controller) Up(ctx context.Context, path string, env []string) (string, error) {
	p, err := config.Load(ctx, path, env)
	if err != nil {
		return "", err
	}
	return p.Name, c.do(ctx, func(context.Context) error {
		if err := c.checkPorts(*p); err != nil {
			return err
		}
		if err := c.store.Up(*p, path); err != nil {
			return err
		}
		c.resetRollouts(*p)
		return nil
	})
}

// resetRollouts lets the next pass resume a paused rollout, as the spec
// promises for every up, even when the file did not change.
func (c *Controller) resetRollouts(p model.Project) {
	for name := range p.Services {
		if m := c.mem[p.Name+"/"+name]; m != nil {
			m.rollout, m.rolledBack = plan.Rollout{}, ""
		}
	}
}

// Scale sets replica overrides.
func (c *Controller) Scale(ctx context.Context, project string, scale map[string]int) error {
	return c.do(ctx, func(context.Context) error { return c.store.Scale(project, scale) })
}

// RolloutRestart rolls every replica of a service and re-pulls its image.
func (c *Controller) RolloutRestart(ctx context.Context, project, service string) error {
	return c.do(ctx, func(context.Context) error {
		if err := c.store.Restart(project, service); err != nil {
			return err
		}
		c.memFor(project + "/" + service).forcePull = true
		return nil
	})
}

// Down drains and removes a project's containers and network and
// unregisters it. Named volumes go too when volumes is true.
func (c *Controller) Down(ctx context.Context, project string, volumes bool) error {
	return c.do(ctx, func(ctx context.Context) error {
		ps, ok := c.store.Get(project)
		if !ok {
			return fmt.Errorf("project %q is not registered", project)
		}
		// Remove first and unregister last, so a failed down can be retried.
		observed, err := c.rt.List(ctx)
		if err != nil {
			return err
		}
		for name, reps := range groupByService(observed[project]) {
			grace := defaultGrace
			if svc, ok := ps.Current.Services[name]; ok {
				grace = svc.StopGrace
			}
			if err := c.removeService(ctx, project+"/"+name, reps, grace); err != nil {
				return err
			}
		}
		for name := range ps.Current.Services {
			c.lb.Close(project + "/" + name)
			delete(c.mem, project+"/"+name)
			c.auto.Forget(project + "/" + name)
		}
		if err := c.rt.RemoveNetwork(ctx, project); err != nil {
			// Best-effort: a network still in use is left for the next down.
			slog.Warn("remove network", "project", project, "err", err)
		}
		if volumes {
			for _, v := range ps.Current.Volumes {
				if err := c.rt.RemoveVolume(ctx, v); err != nil {
					return err
				}
			}
		}
		if err := c.store.Down(project); err != nil {
			return err
		}
		c.record(event{Project: project, Kind: "removed", Text: "project taken down"})
		return nil
	})
}

// Status returns the latest pass's status, limited to project if set.
func (c *Controller) Status(project string) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := Status{RuntimeError: c.status.RuntimeError, Warnings: c.status.Warnings}
	for _, s := range c.status.Services {
		if project == "" || s.Project == project {
			st.Services = append(st.Services, s)
		}
	}
	return st
}

func (c *Controller) watchEvents(ctx context.Context) {
	wait := time.Second
	health := map[string]string{} // container ID → last health event
	for ctx.Err() == nil {
		msgs, errs := c.rt.Events(ctx)
	read:
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-msgs:
				// ponytail: a health change the engine reports without its value is seen on the 10s tick instead.
				if runtime.Ours(e) && runtime.Wakes(e, health) {
					c.Poke()
				}
				wait = time.Second
			case err := <-errs:
				slog.Warn("event stream ended", "err", err)
				break read
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
		c.Poke()
	}
}

func (c *Controller) pass(ctx context.Context) {
	observed, err := c.rt.List(ctx)
	if err != nil {
		// The load balancer keeps serving the last published backends.
		c.mu.Lock()
		c.status.RuntimeError = err.Error()
		c.mu.Unlock()
		return
	}
	st := Status{}
	known := map[string]bool{}
	for _, ps := range c.store.List() {
		p := ps.Current
		known[p.Name] = true
		if err := c.rt.EnsureNetwork(ctx, p.Name); err != nil {
			st.Warnings = append(st.Warnings, fmt.Sprintf("project %s: network: %v", p.Name, err))
			continue
		}
		byService := groupByService(observed[p.Name])
		for _, name := range slices.Sorted(maps.Keys(p.Services)) {
			st.Services = append(st.Services, c.reconcileService(ctx, ps, p.Services[name], byService[name]))
			delete(byService, name)
		}
		for key := range c.mem { // removed while it had no containers: its listener and state go too
			if name, ok := strings.CutPrefix(key, p.Name+"/"); ok && byService[name] == nil {
				if _, inFile := p.Services[name]; !inFile {
					byService[name] = nil
				}
			}
		}
		for name, reps := range byService { // services removed from the file
			if err := c.removeService(ctx, p.Name+"/"+name, reps, defaultGrace); err != nil {
				st.Warnings = append(st.Warnings, fmt.Sprintf("project %s: removing service %s: %v", p.Name, name, err))
			}
		}
	}
	for proj := range observed {
		if !known[proj] {
			st.Warnings = append(st.Warnings, fmt.Sprintf("containers of unregistered project %q are left alone; remove them with podman rm", proj))
		}
	}
	sort.Strings(st.Warnings)
	c.setStatus(st)
}

func (c *Controller) setStatus(st Status) {
	c.mu.Lock()
	c.status = st
	c.mu.Unlock()
}

func (c *Controller) reconcileService(ctx context.Context, ps state.ProjectState, svc model.Service, reps []model.Replica) ServiceStatus {
	key := svc.Project + "/" + svc.Name
	m := c.memFor(key)
	now := time.Now()
	hash := model.WithSalt(svc.Hash, ps.Salt[svc.Name])
	desired := svc.Replicas
	if n, ok := ps.Scale[svc.Name]; ok {
		desired = n
	}
	var auto, autoWarning string
	if svc.Autoscale != nil {
		desired, auto, autoWarning = c.autoscaled(key, svc, m, reps, hash)
	} else {
		c.auto.Forget(key)
	}
	st := ServiceStatus{Project: svc.Project, Service: svc.Name, Desired: desired, Auto: auto, Warnings: serviceWarnings(reps, now)}
	if autoWarning != "" {
		st.Warnings = append(st.Warnings, autoWarning)
	}
	if m.hash != hash { // new config: a paused rollout starts over
		m.hash, m.rollout = hash, plan.Rollout{}
	}
	seen := map[string]bool{}
	for _, r := range reps {
		if m.seenReady[r.ID] || r.Ready() {
			seen[r.ID] = true
		}
	}
	m.seenReady = seen

	out := plan.Plan(plan.Input{Hash: hash, Desired: desired, Update: svc.Update, Replicas: reps, Now: now, Backoff: m.backoff, Rollout: m.rollout, SeenReady: m.seenReady, Since: c.started})
	if until := out.Backoff.Until; until.After(now) && !until.Equal(m.backoff.Until) {
		time.AfterFunc(until.Sub(now), c.Poke) // the tick could leave a 1s backoff at 10s
	}
	m.backoff, m.rollout = out.Backoff, out.Rollout
	if out.Rollback {
		switch ok, err := c.store.Rollback(svc.Project, svc.Name); {
		case err != nil:
			m.rollout.Paused, m.rollout.Reason = true, "rollout failed and the rollback could not be saved: "+err.Error()
		case !ok:
			m.rollout.Paused, m.rollout.Reason = true, "rollout failed and there is no previous version to roll back to"
		default:
			m.rolledBack = "rolled back to the previous version: new replicas failed during rollout"
			slog.Warn("rolled back", "service", key)
			c.Poke()
		}
	}

	actions := out.Actions
	if slices.ContainsFunc(actions, func(a plan.Action) bool { return a.Kind == plan.Create }) && m.pulled != hash {
		if err := c.rt.Pull(ctx, svc.Config.Image, m.forcePull); err != nil {
			// The rollout does not start: keep only removals of dead replicas.
			st.Error = fmt.Sprintf("pull %s: %v", svc.Config.Image, err)
			actions = slices.DeleteFunc(slices.Clone(actions), func(a plan.Action) bool { return a.Kind == plan.Create || a.Drain })
		} else {
			m.pulled, m.forcePull = hash, false
		}
	}

	byID := map[string]model.Replica{}
	for _, r := range reps {
		byID[r.ID] = r
	}
	removing := map[string]bool{}
	for _, a := range actions {
		if a.Kind == plan.Remove {
			removing[a.ReplicaID] = true
		}
	}
	if svc.Expose != nil {
		st.Port = svc.Expose.HostPort
		if err := c.publish(key, svc, m, reps, removing); err != nil && st.Error == "" {
			st.Error = err.Error()
		}
	} else if m.lb != nil {
		c.lb.Close(key)
		m.lb = nil
	}

	event := func(kind, replica, text string) {
		c.record(event{Project: svc.Project, Service: svc.Name, Replica: replica, Kind: kind, Text: text})
	}
	for _, a := range actions {
		switch a.Kind {
		case plan.Remove:
			r := byID[a.ReplicaID]
			if !a.Drain {
				// Removing a dead replica deletes its logs; keep the tail.
				logs := c.rt.Logs(ctx, r.ID, 20)
				why := "state " + r.State
				if r.Health != "" {
					why += ", health " + r.Health
				}
				event("replaced", r.Name, why+"; last log lines:\n"+logs)
			}
			if a.Drain && m.lb != nil {
				dctx, cancel := context.WithTimeout(ctx, svc.StopGrace)
				m.lb.Drain(dctx, a.ReplicaID)
				cancel()
			}
			if err := c.rt.Remove(ctx, a.ReplicaID, svc.StopGrace); err != nil {
				st.Error = "remove: " + err.Error()
			} else if a.Drain {
				event("removed", r.Name, "drained and removed (scale down or rollout)")
			}
		case plan.Create:
			for range a.Count {
				name, err := c.rt.Create(ctx, svc, hash)
				if err != nil {
					st.Error = "create: " + err.Error()
					break
				}
				event("created", name, "started")
			}
		}
	}

	countReplicas(&st, reps, hash, removing)
	st.Paused, st.Reason, st.RolledBack = m.rollout.Paused, m.rollout.Reason, m.rolledBack
	if m.backoff.Until.After(now) {
		st.BackoffUntil = m.backoff.Until
	}
	cur := snap(st, reps)
	for _, e := range changes(m.seen, cur, svc.Project, svc.Name) {
		c.record(e)
	}
	m.seen = cur
	return st
}

// event is one thing the controller did and why.
type event struct{ Project, Service, Replica, Kind, Text string }

// record logs e, so journald shows what the controller did.
func (c *Controller) record(e event) {
	level := slog.LevelInfo
	switch e.Kind {
	case "replaced", "paused", "rolled-back", "error", "crash-backoff":
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, e.Kind, "project", e.Project, "service", e.Service, "replica", e.Replica, "text", e.Text)
}

// snapshot is what one pass saw of a service.
type snapshot struct {
	ok                 bool // false before the first pass
	desired, outdated  int
	auto               string
	paused             bool
	reason, rolledBack string
	err                string
	backoffUntil       time.Time
	health             map[string]string // running replica name → health
}

func snap(st ServiceStatus, reps []model.Replica) snapshot {
	s := snapshot{ok: true, desired: st.Desired, outdated: st.Outdated, auto: st.Auto, paused: st.Paused, reason: st.Reason,
		rolledBack: st.RolledBack, err: st.Error, backoffUntil: st.BackoffUntil, health: map[string]string{}}
	for _, r := range reps {
		if r.State == "running" {
			s.health[r.Name] = r.Health
		}
	}
	return s
}

// changes lists the events between two passes of one service.
func changes(old, cur snapshot, project, service string) []event {
	if !old.ok {
		return nil
	}
	var out []event
	add := func(kind, replica, text string) {
		out = append(out, event{Project: project, Service: service, Replica: replica, Kind: kind, Text: text})
	}
	if cur.desired != old.desired {
		text := fmt.Sprintf("desired replicas %d → %d", old.desired, cur.desired)
		if cur.auto != "" {
			text += " (autoscale " + cur.auto + ")"
		}
		add("scaled", "", text)
	}
	if old.outdated == 0 && cur.outdated > 0 {
		add("rollout-started", "", fmt.Sprintf("%d replicas run an old config", cur.outdated))
	}
	if old.outdated > 0 && cur.outdated == 0 && !cur.paused {
		add("rollout-done", "", "every replica runs the current config")
	}
	if cur.paused && !old.paused {
		add("paused", "", cur.reason)
	}
	if cur.rolledBack != "" && cur.rolledBack != old.rolledBack {
		add("rolled-back", "", cur.rolledBack)
	}
	if cur.err != "" && cur.err != old.err {
		add("error", "", cur.err)
	}
	if !cur.backoffUntil.IsZero() && !cur.backoffUntil.Equal(old.backoffUntil) {
		add("crash-backoff", "", "no new replicas until "+cur.backoffUntil.Local().Format(time.TimeOnly))
	}
	for _, name := range slices.Sorted(maps.Keys(cur.health)) {
		if was, ok := old.health[name]; ok && was != cur.health[name] {
			add("health", name, healthLabel(was)+" → "+healthLabel(cur.health[name]))
		}
	}
	return out
}

func healthLabel(h string) string {
	if h == "" {
		return "running"
	}
	return h
}

func countReplicas(st *ServiceStatus, reps []model.Replica, hash string, removing map[string]bool) {
	for _, r := range reps {
		if removing[r.ID] {
			continue
		}
		if r.Ready() {
			st.Serving++
		}
		if r.Hash != hash {
			st.Outdated++
		} else if r.Ready() {
			st.Ready++
		}
	}
}

// autoscaled feeds the sampler and returns its desired count, the status
// text and any warning. The first time it sees a service it starts from the
// replica count, dead ones included (they died while anastasis was down),
// falling back to deploy.replicas for a new service.
// ponytail: mid start-first rollout this counts up to parallelism extra; the scale-down window trims them.
func (c *Controller) autoscaled(key string, svc model.Service, m *memory, reps []model.Replica, hash string) (int, string, string) {
	var ready []string
	rollout := false
	for _, r := range reps {
		if r.Hash != hash {
			rollout = true
		} else if r.Ready() {
			ready = append(ready, r.ID)
		}
	}
	initial := len(reps)
	if initial == 0 {
		initial = svc.Replicas
	}
	c.auto.Track(key, *svc.Autoscale, m.lb, ready, initial, rollout, m.rollout.Paused)
	n, reason, warning, _ := c.auto.Desired(key)
	return n, fmt.Sprintf("%d→%d (%s)", len(ready), n, reason), warning
}

// lbName names a service's balancer, and so its sticky cookie, after the
// project too: browsers share cookies across ports, so two projects' "web"
// services must not share one. "." separates: "_" occurs in both names.
func lbName(key string) string { return strings.ReplaceAll(key, "/", ".") } // no project name has a dot

// publish points the service's load balancer at its ready replicas and
// makes sure it listens. A failed listen is retried on the next pass.
func (c *Controller) publish(key string, svc model.Service, m *memory, reps []model.Replica, removing map[string]bool) error {
	if m.lb == nil || m.lb.Sticky() != svc.Sticky {
		m.lb = lb.NewService(lbName(key), svc.Sticky)
	}
	var bs []lb.Backend
	for _, r := range reps {
		if r.Ready() && r.HostPort > 0 && !removing[r.ID] {
			bs = append(bs, lb.Backend{ID: r.ID, Addr: fmt.Sprintf("127.0.0.1:%d", r.HostPort)})
		}
	}
	m.lb.SetBackends(bs)
	if err := c.lb.Listen(key, svc.Expose.HostPort, m.lb); err != nil {
		return fmt.Errorf("listen on port %d: %w", svc.Expose.HostPort, err)
	}
	return nil
}

// removeService drains and removes a service's containers and returns the
// first removal error.
func (c *Controller) removeService(ctx context.Context, key string, reps []model.Replica, grace time.Duration) error {
	if m := c.mem[key]; m != nil && m.lb != nil {
		m.lb.SetBackends(nil)
		for _, r := range reps {
			dctx, cancel := context.WithTimeout(ctx, grace)
			m.lb.Drain(dctx, r.ID)
			cancel()
		}
	}
	c.lb.Close(key)
	var first error
	for _, r := range reps {
		if err := c.rt.Remove(ctx, r.ID, grace); err != nil {
			slog.Warn("remove container", "container", r.Name, "err", err)
			if first == nil {
				first = err
			}
		}
	}
	delete(c.mem, key)
	c.auto.Forget(key)
	return first
}

// checkPorts fails if an exposed host port is taken by anything other
// than the same service's own listener.
func (c *Controller) checkPorts(p model.Project) error {
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		svc := p.Services[name]
		if svc.Expose == nil {
			continue
		}
		key, port := p.Name+"/"+name, svc.Expose.HostPort
		if owner, ok := c.lb.Owner(port); ok {
			if owner != key {
				return fmt.Errorf("service %q: host port %d is already used by %s", name, port, owner)
			}
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return fmt.Errorf("service %q: host port %d: %w", name, port, err)
		}
		ln.Close()
	}
	return nil
}

func (c *Controller) memFor(key string) *memory {
	m := c.mem[key]
	if m == nil {
		m = &memory{}
		c.mem[key] = m
	}
	return m
}

func groupByService(reps []model.Replica) map[string][]model.Replica {
	out := map[string][]model.Replica{}
	for _, r := range reps {
		out[r.Service] = append(out[r.Service], r)
	}
	return out
}

// serviceWarnings explains states that look stuck.
func serviceWarnings(reps []model.Replica, now time.Time) []string {
	var w []string
	if slices.ContainsFunc(reps, func(r model.Replica) bool { return r.State == "running" && r.Health == "" }) {
		w = append(w, "no healthcheck: replicas get traffic as soon as they start")
	}
	for _, r := range reps {
		if r.Health == "starting" && now.Sub(r.Started) > slowHealthAge {
			w = append(w, fmt.Sprintf("%s: healthcheck has not reported for %s; rootless Podman runs healthchecks through systemd user timers, so check `loginctl enable-linger`", r.Name, now.Sub(r.Started).Round(time.Second)))
		}
		if r.Health == "stalled" {
			w = append(w, r.Name+": healthcheck is overdue (the app or the check hangs); no traffic until it answers, replaced if it stays stuck")
		}
	}
	return w
}
