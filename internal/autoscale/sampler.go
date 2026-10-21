package autoscale

import (
	"context"
	"sync"
	"time"

	"github.com/rationislabs/anastasis/internal/lb"
	"github.com/rationislabs/anastasis/internal/model"
	"github.com/rationislabs/anastasis/internal/runtime"
)

const sampleEvery = 2 * time.Second

// Sampler measures autoscaled services and publishes their desired counts.
// The reconcile loop feeds it with Track and reads Desired; slow stats calls
// happen here, never on the reconcile loop.
type Sampler struct {
	rt      *runtime.Runtime
	windows Windows

	mu   sync.Mutex
	svcs map[string]*tracked // key "project/service"

	cpuPrev map[string]map[string]cpuSample // check goroutine only
}

type tracked struct {
	cfg      model.Autoscale
	lb       *lb.Service // nil until the service's load balancer exists
	ready    []string    // ready, up-to-date replica IDs
	rollout  bool
	paused   bool
	inflight []float64 // ring of total in-flight samples
	since    time.Time
	history  []Rec
	lastDown time.Time
	desired  int
	reason   string
	warning  string
}

type cpuSample struct {
	usage time.Duration
	at    time.Time
}

// NewSampler returns a sampler; call Run to start it.
func NewSampler(rt *runtime.Runtime, w Windows) *Sampler {
	return &Sampler{rt: rt, windows: w, svcs: map[string]*tracked{}, cpuPrev: map[string]map[string]cpuSample{}}
}

// Track updates what the sampler knows about one autoscaled service. On
// the first call the desired count starts at initial (the running count,
// so a anastasis restart never shrinks a loaded service).
func (s *Sampler) Track(key string, cfg model.Autoscale, lbSvc *lb.Service, ready []string, initial int, rollout, paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.svcs[key]
	if t == nil {
		t = &tracked{since: time.Now(), desired: initial, reason: "waiting for data"}
		s.svcs[key] = t
	}
	if t.lb != lbSvc {
		t.inflight = nil
	}
	t.cfg, t.lb, t.ready, t.rollout, t.paused = cfg, lbSvc, ready, rollout, paused
	t.desired = clamp(t.desired, cfg.Min, cfg.Max) // new bounds apply at once
}

// Desired returns the current desired count for key.
func (s *Sampler) Desired(key string) (n int, reason, warning string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.svcs[key]
	if t == nil {
		return 0, "", "", false
	}
	return t.desired, t.reason, t.warning, true
}

// Forget drops a service that is gone or no longer autoscaled.
func (s *Sampler) Forget(key string) {
	s.mu.Lock()
	delete(s.svcs, key)
	s.mu.Unlock()
}

// Run samples and decides until ctx ends, calling changed whenever a
// check changes a desired count.
func (s *Sampler) Run(ctx context.Context, changed func()) {
	// In-flight sampling has its own goroutine: stats calls in check can
	// take up to their API timeout each.
	// ponytail: stats calls stay serial, so a hung runtime delays decisions by up to 30s per replica; it cannot start containers then anyway.
	go func() {
		sample := time.NewTicker(sampleEvery)
		defer sample.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sample.C:
				s.sampleInflight()
			}
		}
	}()
	check := time.NewTicker(s.windows.Check)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			if s.check(ctx) {
				changed()
			}
		}
	}
}

func (s *Sampler) sampleInflight() {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := max(int(s.windows.Average/sampleEvery), 1)
	for _, t := range s.svcs {
		if t.lb == nil {
			continue
		}
		t.inflight = append(t.inflight, float64(t.lb.Inflight()))
		if len(t.inflight) > keep {
			t.inflight = t.inflight[len(t.inflight)-keep:]
		}
	}
}

type cpuJob struct {
	ids   []string
	limit float64
}

// check decides every service and reports whether a desired count changed.
func (s *Sampler) check(ctx context.Context) bool {
	s.mu.Lock()
	jobs := map[string]cpuJob{}
	for key, t := range s.svcs {
		if t.cfg.CPU > 0 {
			jobs[key] = cpuJob{ids: t.ready, limit: t.cfg.CPULimit}
		}
	}
	s.mu.Unlock()

	// Stats calls run without the lock.
	type cpuResult struct {
		pct float64
		err string
	}
	cpu := map[string]cpuResult{}
	for key, j := range jobs {
		pct, err := s.measureCPU(ctx, key, j)
		cpu[key] = cpuResult{pct, err}
	}
	for key := range s.cpuPrev {
		if _, ok := jobs[key]; !ok {
			delete(s.cpuPrev, key)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now, changed := time.Now(), false
	for key, t := range s.svcs {
		in := Input{Config: t.cfg, Current: t.desired, Ready: len(t.ready), Inflight: -1, CPU: -1,
			History: t.history, LastDown: t.lastDown, Since: t.since, Rollout: t.rollout, Paused: t.paused, Now: now, Windows: s.windows}
		if len(t.inflight) > 0 {
			in.Inflight = average(t.inflight)
		}
		if r, ok := cpu[key]; ok {
			in.CPU, in.CPUErr = r.pct, r.err
		}
		d := Decide(in)
		changed = changed || d.Desired != t.desired
		t.desired, t.history, t.lastDown, t.reason, t.warning = d.Desired, d.History, d.LastDown, d.Reason, d.Warning
	}
	return changed
}

// measureCPU returns the average CPU percent of the limit across replicas
// that have two samples, or -1 and the reason when none do.
func (s *Sampler) measureCPU(ctx context.Context, key string, j cpuJob) (float64, string) {
	prev := s.cpuPrev[key]
	next := map[string]cpuSample{}
	var pcts []float64
	reason := "waiting for a second sample"
	for _, id := range j.ids {
		usage, at, err := s.rt.Stats(ctx, id)
		if err != nil {
			reason = err.Error()
			continue
		}
		cur := cpuSample{usage: usage, at: at}
		next[id] = cur
		if pct, ok := cpuPercent(prev[id], cur, j.limit); ok {
			pcts = append(pcts, pct)
		}
	}
	s.cpuPrev[key] = next
	if len(pcts) == 0 {
		return -1, reason
	}
	return average(pcts), ""
}

// cpuPercent is cores used between two samples as a percent of limit cores.
// It is not ok without a usable interval: no previous sample, a counter
// that went down (an in-place restart resets it) or no time between them.
func cpuPercent(prev, cur cpuSample, limit float64) (float64, bool) {
	wall := cur.at.Sub(prev.at).Seconds()
	if prev.at.IsZero() || cur.usage < prev.usage || wall <= 0 {
		return 0, false
	}
	if limit <= 0 {
		return 0, true
	}
	return (cur.usage - prev.usage).Seconds() / wall / limit * 100, true
}

func average(xs []float64) float64 {
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
