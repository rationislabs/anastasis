// Package autoscale sets replica counts from load. Decide is pure; the
// Sampler gathers its inputs.
package autoscale

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
)

// Windows are the autoscaler's timing constants. Only tests change them.
type Windows struct {
	Check   time.Duration // how often a decision is made
	Average time.Duration // in-flight samples are averaged over this window
	Down    time.Duration // scale-down uses the highest recommendation in this window
	Step    time.Duration // at most one replica is removed per Step
}

// DefaultWindows are the values from the spec.
var DefaultWindows = Windows{Check: 15 * time.Second, Average: 30 * time.Second, Down: 5 * time.Minute, Step: time.Minute}

const tolerance = 0.1

// Rec is one past recommendation.
type Rec struct {
	At time.Time
	N  int
}

// Input is everything Decide needs for one service.
type Input struct {
	Config   model.Autoscale
	Current  int     // the count the service is scaled to now
	Ready    int     // ready, up-to-date replicas
	Inflight float64 // average total in-flight requests; < 0 means no data
	CPU      float64 // average CPU percent of the limit per ready replica; < 0 means no data
	CPUErr   string  // why CPU data is missing
	History  []Rec
	LastDown time.Time
	Since    time.Time // when the autoscaler started watching this service
	Rollout  bool
	Paused   bool // the rollout is paused until the next up or resume
	Now      time.Time
	Windows  Windows
}

// Decision is the new desired count plus state to carry to the next check.
type Decision struct {
	Desired  int
	History  []Rec
	LastDown time.Time
	Reason   string // for anastasis status, e.g. "inflight 14.0/10, cpu 55/70"
	Warning  string
}

// Decide returns the desired replica count. Scaling up is immediate;
// scaling down waits for a quiet window and goes one replica at a time;
// missing data never scales down.
func Decide(in Input) Decision {
	cfg := in.Config
	d := Decision{Desired: clamp(in.Current, cfg.Min, cfg.Max), LastDown: in.LastDown}
	for _, r := range in.History {
		if in.Now.Sub(r.At) < in.Windows.Down {
			d.History = append(d.History, r)
		}
	}
	// A hold records the current count, so a gap (a long rollout, no
	// ready replicas, no data) still counts toward the scale-down window.
	hold := func(reason string) Decision {
		d.History = append(d.History, Rec{At: in.Now, N: d.Desired})
		d.Reason = reason
		return d
	}
	if in.Rollout && in.Paused {
		return hold("frozen while the rollout is paused")
	}
	if in.Rollout {
		return hold("frozen during rollout")
	}
	if in.Ready == 0 {
		return hold("no ready replicas")
	}

	rec, canDown := -1, true
	var reasons []string
	if cfg.Inflight > 0 {
		if in.Inflight >= 0 {
			per := in.Inflight / float64(in.Ready)
			rec = max(rec, recommend(in.Ready, per, cfg.Inflight))
			reasons = append(reasons, fmt.Sprintf("inflight %.1f/%g", per, cfg.Inflight))
		} else {
			canDown = false
		}
	}
	if cfg.CPU > 0 {
		if in.CPU >= 0 {
			rec = max(rec, recommend(in.Ready, in.CPU, cfg.CPU))
			reasons = append(reasons, fmt.Sprintf("cpu %.0f/%g", in.CPU, cfg.CPU))
		} else {
			canDown = false
			d.Warning = "cpu signal unavailable: " + in.CPUErr
		}
	}
	d.Reason = strings.Join(reasons, ", ")
	if rec < 0 { // no signal has data
		return hold("waiting for data")
	}
	rec = clamp(rec, cfg.Min, cfg.Max)
	d.History = append(d.History, Rec{At: in.Now, N: rec})

	switch {
	case rec > d.Desired:
		d.Desired = rec
	case rec < d.Desired && canDown:
		highest := rec
		for _, r := range d.History {
			highest = max(highest, r.N)
		}
		if highest < d.Desired && in.Now.Sub(in.Since) >= in.Windows.Down && in.Now.Sub(in.LastDown) >= in.Windows.Step {
			d.Desired--
			d.LastDown = in.Now
		}
	}
	return d
}

// recommend is the Kubernetes HPA formula with a tolerance band.
func recommend(ready int, observed, target float64) int {
	ratio := observed / target
	if math.Abs(ratio-1) <= tolerance {
		return ready
	}
	return int(math.Ceil(float64(ready) * ratio))
}

func clamp(n, lo, hi int) int { return min(max(n, lo), hi) }
