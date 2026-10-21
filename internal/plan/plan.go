// Package plan decides what to do with one service's replicas. It does no
// I/O: given what should run and what runs, it returns actions.
package plan

import (
	"slices"
	"sort"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
)

const (
	crashWindow = 30 * time.Second // dying sooner than this after start is a crash
	maxBackoff  = 5 * time.Minute
	quietReset  = 5 * time.Minute // backoff resets after this long without a crash
)

// Backoff is a service's crash-loop state.
type Backoff struct {
	Delay time.Duration // 0 when not backing off
	Until time.Time     // no new replicas before this
}

// Rollout is a service's rolling-update state.
type Rollout struct {
	Paused   bool
	Reason   string
	LastStep time.Time // when the last batch of new replicas was started
}

// Input is everything Plan needs for one service.
type Input struct {
	Hash      string // current config hash, salt included
	Desired   int
	Update    model.UpdateConfig
	Replicas  []model.Replica
	Now       time.Time
	Backoff   Backoff
	Rollout   Rollout
	SeenReady map[string]bool // replica IDs that were ever ready
	Since     time.Time       // when anastasis started watching; see Plan
}

// Kind is what an Action does.
type Kind int

const (
	Create Kind = iota
	Remove
)

// Action is one step for the reconcile loop to execute.
type Action struct {
	Kind      Kind
	Count     int    // Create: how many replicas
	ReplicaID string // Remove: which replica
	Drain     bool   // Remove: take it out of the load balancer and wait first
}

// Output is Plan's decision plus the updated per-service state.
type Output struct {
	Actions  []Action
	Backoff  Backoff
	Rollout  Rollout
	Rollback bool // failure_action rollback fired; the caller swaps configs
}

// Plan returns the actions that move a service toward its desired state.
func Plan(in Input) Output {
	out := Output{Backoff: in.Backoff, Rollout: in.Rollout}
	var current, outdated []model.Replica
	crashed, failed := false, false
	for _, r := range in.Replicas {
		upToDate := r.Hash == in.Hash
		if r.Dead() {
			out.Actions = append(out.Actions, Action{Kind: Remove, ReplicaID: r.ID})
			if r.State != "running" && r.Finished.Before(in.Since) {
				continue // died unwatched: a host shutdown kills young replicas too
			}
			if r.State != "running" && r.Finished.Sub(r.Started) < crashWindow {
				crashed = true
			}
			// A new replica fails the rollout if it dies within monitor of
			// starting, or dies without ever having been ready (a slow
			// healthcheck can outlast monitor).
			// ponytail: monitor counts from container start, not first healthy; track first-healthy time if that proves too coarse.
			if upToDate && (r.Started.IsZero() || in.Now.Sub(r.Started) < in.Update.Monitor || !in.SeenReady[r.ID]) {
				failed = true
			}
			continue
		}
		if upToDate {
			current = append(current, r)
		} else {
			outdated = append(outdated, r)
		}
	}
	out.Backoff = nextBackoff(in.Backoff, crashed, in.Now)
	canCreate := !in.Now.Before(out.Backoff.Until)

	// A rollout lasts until its last step has been monitored, so a
	// stop-first rollout, which removes the old replicas first, can fail too.
	rolling := len(outdated) > 0 || !out.Rollout.LastStep.IsZero() && in.Now.Sub(out.Rollout.LastStep) < in.Update.Monitor
	if failed && rolling && !out.Rollout.Paused {
		switch in.Update.FailureAction {
		case "rollback":
			out.Rollback = true
		case "continue":
		default:
			out.Rollout.Paused = true
			out.Rollout.Reason = "new replicas failed during rollout"
		}
	}

	if len(outdated) == 0 {
		if out.Rollout.Paused {
			// The old replicas are gone. Never fall back to creating the
			// config that just failed; a new up or rollout restart resumes.
			return out
		}
		if out.Rollback {
			return out
		}
		if !rolling {
			out.Rollout = Rollout{}
		}
		switch {
		case len(current) < in.Desired && canCreate:
			out.Actions = append(out.Actions, Action{Kind: Create, Count: in.Desired - len(current)})
		case len(current) > in.Desired:
			for _, r := range removalOrder(current)[:len(current)-in.Desired] {
				out.Actions = append(out.Actions, Action{Kind: Remove, ReplicaID: r.ID, Drain: true})
			}
		}
		return out
	}
	if out.Rollout.Paused || out.Rollback {
		return out
	}

	sort.SliceStable(outdated, func(i, j int) bool { return outdated[i].Started.Before(outdated[j].Started) })
	removeOld := func(n int) {
		for _, r := range outdated[:min(n, len(outdated))] {
			out.Actions = append(out.Actions, Action{Kind: Remove, ReplicaID: r.ID, Drain: true})
		}
	}
	if extra := len(current) - in.Desired; extra > 0 { // scaled down mid-rollout
		ordered := removalOrder(current)
		for _, r := range ordered[:extra] {
			out.Actions = append(out.Actions, Action{Kind: Remove, ReplicaID: r.ID, Drain: true})
		}
		current = ordered[extra:] // old replicas below are only retired against what is left ready
	}
	ready := 0
	for _, r := range current {
		if r.Ready() {
			ready++
		}
	}
	waiting := len(current) - ready
	stepDue := !in.Now.Before(out.Rollout.LastStep.Add(in.Update.Delay))
	need := in.Desired - len(current)

	if in.Update.Order == "stop-first" {
		if waiting == 0 && stepDue && canCreate {
			if need == 0 {
				removeOld(len(outdated))
				return out
			}
			n := min(in.Update.Parallelism, need)
			removeOld(n)
			out.Actions = append(out.Actions, Action{Kind: Create, Count: n})
			out.Rollout.LastStep = in.Now
		}
		return out
	}

	// start-first: retire old replicas as new ones become ready, then start
	// the next batch. Total replicas never exceed Desired + Parallelism.
	if surplus := ready + len(outdated) - in.Desired; surplus > 0 {
		removeOld(surplus)
	}
	if waiting == 0 && need > 0 && stepDue && canCreate {
		out.Actions = append(out.Actions, Action{Kind: Create, Count: min(in.Update.Parallelism, need)})
		out.Rollout.LastStep = in.Now
	}
	return out
}

func nextBackoff(b Backoff, crashed bool, now time.Time) Backoff {
	if crashed {
		d := min(max(b.Delay*2, time.Second), maxBackoff)
		return Backoff{Delay: d, Until: now.Add(d)}
	}
	if b.Delay > 0 && now.Sub(b.Until) >= quietReset {
		return Backoff{}
	}
	return b
}

// removalOrder puts the cheapest replicas to lose first: not ready, then newest.
func removalOrder(rs []model.Replica) []model.Replica {
	out := slices.Clone(rs)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ready() != out[j].Ready() {
			return !out[i].Ready()
		}
		return out[i].Started.After(out[j].Started)
	})
	return out
}
