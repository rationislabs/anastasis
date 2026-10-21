package plan

import (
	"reflect"
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

const h = time.Hour

func running(id, hash, health string, age time.Duration) model.Replica {
	return model.Replica{ID: id, Hash: hash, State: "running", Health: health, Started: now.Add(-age)}
}

func exited(id, hash string, age, lived time.Duration) model.Replica {
	s := now.Add(-age)
	return model.Replica{ID: id, Hash: hash, State: "exited", Started: s, Finished: s.Add(lived)}
}

func create(n int) Action     { return Action{Kind: Create, Count: n} }
func remove(id string) Action { return Action{Kind: Remove, ReplicaID: id} }
func drain(id string) Action  { return Action{Kind: Remove, ReplicaID: id, Drain: true} }

func acts(a ...Action) []Action { return a }

var (
	upd       = model.UpdateConfig{Parallelism: 1, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "pause"}
	threeOld  = []model.Replica{running("o1", "old", "healthy", 3*h), running("o2", "old", "healthy", 2*h), running("o3", "old", "healthy", h)}
	crashNew  = exited("n1", "new", 10*time.Second, 5*time.Second)
	firstBack = Backoff{Delay: time.Second, Until: now.Add(time.Second)}
)

func stopFirst(failure string) model.UpdateConfig {
	return model.UpdateConfig{Parallelism: 1, Monitor: 30 * time.Second, Order: "stop-first", FailureAction: failure}
}

func with(rs ...[]model.Replica) []model.Replica {
	var out []model.Replica
	for _, r := range rs {
		out = append(out, r...)
	}
	return out
}

func one(r model.Replica) []model.Replica { return []model.Replica{r} }

func TestPlan(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want Output
	}{
		{"scale up from zero", Input{Desired: 3}, Output{Actions: acts(create(3))}},
		{"steady state", Input{Desired: 2, Replicas: []model.Replica{running("a", "new", "healthy", h), running("b", "new", "", h)}}, Output{}},
		{"scale down removes not-ready, then newest", Input{Desired: 1, Replicas: []model.Replica{
			running("a", "new", "healthy", 3*h), running("b", "new", "healthy", 2*h), running("c", "new", "starting", h)}},
			Output{Actions: acts(drain("c"), drain("b"))}},
		{"scale to zero", Input{Desired: 0, Replicas: []model.Replica{running("a", "new", "healthy", 2*h), running("b", "new", "healthy", h)}},
			Output{Actions: acts(drain("b"), drain("a"))}},
		{"replace dead replica", Input{Desired: 2, Replicas: []model.Replica{running("a", "new", "healthy", 2*h), exited("b", "new", 2*h, h)}},
			Output{Actions: acts(remove("b"), create(1))}},
		{"replace unhealthy replica", Input{Desired: 1, Replicas: one(running("a", "new", "unhealthy", h))},
			Output{Actions: acts(remove("a"), create(1))}},
		{"crash starts backoff", Input{Desired: 1, Replicas: one(exited("b", "new", time.Minute, 5*time.Second))},
			Output{Actions: acts(remove("b")), Backoff: firstBack}},
		{"backoff doubles", Input{Desired: 1, Backoff: Backoff{Delay: 4 * time.Second, Until: now.Add(-time.Second)}, Replicas: one(exited("b", "new", time.Minute, 5*time.Second))},
			Output{Actions: acts(remove("b")), Backoff: Backoff{Delay: 8 * time.Second, Until: now.Add(8 * time.Second)}}},
		{"backoff caps at 5m", Input{Desired: 1, Backoff: Backoff{Delay: 4 * time.Minute, Until: now.Add(-time.Second)}, Replicas: one(exited("b", "new", time.Minute, 5*time.Second))},
			Output{Actions: acts(remove("b")), Backoff: Backoff{Delay: 5 * time.Minute, Until: now.Add(5 * time.Minute)}}},
		{"backoff blocks create", Input{Desired: 2, Backoff: Backoff{Delay: 2 * time.Second, Until: now.Add(time.Second)}, Replicas: one(running("a", "new", "healthy", h))},
			Output{Backoff: Backoff{Delay: 2 * time.Second, Until: now.Add(time.Second)}}},
		{"backoff resets after 5 quiet minutes", Input{Desired: 1, Backoff: Backoff{Delay: 8 * time.Second, Until: now.Add(-6 * time.Minute)}, Replicas: one(running("a", "new", "healthy", h))},
			Output{}},
		{"start-first adds first new replica", Input{Desired: 3, Replicas: threeOld},
			Output{Actions: acts(create(1)), Rollout: Rollout{LastStep: now}}},
		{"start-first waits for new replica", Input{Desired: 3, Replicas: with(one(running("n1", "new", "starting", 10*time.Second)), threeOld)},
			Output{}},
		{"start-first swaps when new is ready", Input{Desired: 3, Replicas: with(one(running("n1", "new", "healthy", 10*time.Second)), threeOld)},
			Output{Actions: acts(drain("o1"), create(1)), Rollout: Rollout{LastStep: now}}},
		{"delay holds next batch", Input{Desired: 3, Update: model.UpdateConfig{Parallelism: 1, Delay: 5 * time.Second, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "pause"},
			Rollout: Rollout{LastStep: now.Add(-time.Second)}, Replicas: with(one(running("n1", "new", "healthy", 10*time.Second)), threeOld)},
			Output{Actions: acts(drain("o1")), Rollout: Rollout{LastStep: now.Add(-time.Second)}}},
		{"stop-first replaces one at a time", Input{Desired: 3, Update: model.UpdateConfig{Parallelism: 1, Monitor: 30 * time.Second, Order: "stop-first", FailureAction: "pause"}, Replicas: threeOld},
			Output{Actions: acts(drain("o1"), create(1)), Rollout: Rollout{LastStep: now}}},
		{"death before anastasis started is no crash (host shutdown)", Input{Desired: 1, Since: now.Add(-time.Second), Replicas: one(crashNew)},
			Output{Actions: acts(remove("n1"), create(1))}},
		{"death before anastasis started fails no rollout", Input{Desired: 3, Since: now.Add(-time.Second), Replicas: with(one(crashNew), threeOld)},
			Output{Actions: acts(remove("n1"), create(1)), Rollout: Rollout{LastStep: now}}},
		{"failed rollout pauses", Input{Desired: 3, Replicas: with(one(crashNew), threeOld)},
			Output{Actions: acts(remove("n1")), Backoff: firstBack, Rollout: Rollout{Paused: true, Reason: "new replicas failed during rollout"}}},
		{"failed rollout rolls back", Input{Desired: 3, Update: model.UpdateConfig{Parallelism: 1, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "rollback"}, Replicas: with(one(crashNew), threeOld)},
			Output{Actions: acts(remove("n1")), Backoff: firstBack, Rollback: true}},
		{"failure_action continue keeps going", Input{Desired: 3, Update: model.UpdateConfig{Parallelism: 1, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "continue"}, Replicas: with(one(crashNew), threeOld)},
			Output{Actions: acts(remove("n1")), Backoff: firstBack}},
		{"paused rollout holds", Input{Desired: 3, Rollout: Rollout{Paused: true, Reason: "x"}, Replicas: threeOld},
			Output{Rollout: Rollout{Paused: true, Reason: "x"}}},
		{"pause holds after old replicas are gone", Input{Desired: 3, Rollout: Rollout{Paused: true, Reason: "x"}, Replicas: one(exited("o1", "old", 3*h, 2*h))},
			Output{Actions: acts(remove("o1")), Rollout: Rollout{Paused: true, Reason: "x"}}},
		{"slow failure of a never-ready replica pauses", Input{Desired: 3, Replicas: with(one(running("n1", "new", "unhealthy", 95*time.Second)), threeOld)},
			Output{Actions: acts(remove("n1")), Rollout: Rollout{Paused: true, Reason: "new replicas failed during rollout"}}},
		{"late death of a replica that was ready is self-healed", Input{Desired: 3, SeenReady: map[string]bool{"n1": true}, Replicas: with(one(running("n1", "new", "unhealthy", 95*time.Second)), threeOld)},
			Output{Actions: acts(remove("n1"), create(1)), Rollout: Rollout{LastStep: now}}},
		{"scale down mid-rollout keeps ready capacity", Input{Desired: 1, Update: model.UpdateConfig{Parallelism: 2, Monitor: 30 * time.Second, Order: "start-first", FailureAction: "pause"},
			Replicas: with(threeOld, []model.Replica{running("n1", "new", "starting", 20*time.Second), running("n2", "new", "starting", 10*time.Second)})},
			Output{Actions: acts(drain("n2"), drain("o1"), drain("o2"))}},
		{"stop-first single replica that fails pauses", Input{Desired: 1, Update: stopFirst("pause"), Rollout: Rollout{LastStep: now.Add(-10 * time.Second)}, Replicas: one(crashNew)},
			Output{Actions: acts(remove("n1")), Backoff: firstBack, Rollout: Rollout{Paused: true, Reason: "new replicas failed during rollout", LastStep: now.Add(-10 * time.Second)}}},
		{"stop-first single replica that fails rolls back", Input{Desired: 1, Update: stopFirst("rollback"), Rollout: Rollout{LastStep: now.Add(-10 * time.Second)}, Replicas: one(crashNew)},
			Output{Actions: acts(remove("n1")), Backoff: firstBack, Rollout: Rollout{LastStep: now.Add(-10 * time.Second)}, Rollback: true}},
		{"rollout stays monitored after the old replicas are gone", Input{Desired: 1, Update: stopFirst("pause"), Rollout: Rollout{LastStep: now.Add(-10 * time.Second)}, Replicas: one(running("n1", "new", "healthy", 10*time.Second))},
			Output{Rollout: Rollout{LastStep: now.Add(-10 * time.Second)}}},
		{"rollout ends after monitor", Input{Desired: 1, Update: stopFirst("pause"), Rollout: Rollout{LastStep: now.Add(-time.Minute)}, Replicas: one(running("n1", "new", "healthy", time.Minute))},
			Output{}},
		{"scale down mid-rollout", Input{Desired: 1, Replicas: []model.Replica{
			running("n1", "new", "healthy", time.Minute), running("n2", "new", "healthy", 2*time.Minute),
			running("o1", "old", "healthy", 3*h), running("o2", "old", "healthy", 2*h)}},
			Output{Actions: acts(drain("n1"), drain("o1"), drain("o2"))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			in.Now = now
			in.Hash = "new"
			if in.Update == (model.UpdateConfig{}) {
				in.Update = upd
			}
			if got := Plan(in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("\ngot  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
