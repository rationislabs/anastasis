package autoscale

import (
	"testing"
	"time"

	"github.com/rationislabs/anastasis/internal/model"
)

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

var (
	inflightOnly = model.Autoscale{Min: 1, Max: 6, Inflight: 10}
	cpuOnly      = model.Autoscale{Min: 1, Max: 6, CPU: 70, CPULimit: 0.5}
	both         = model.Autoscale{Min: 1, Max: 6, Inflight: 10, CPU: 70, CPULimit: 0.5}
)

func TestDecide(t *testing.T) {
	tests := []struct {
		name         string
		in           Input
		want         int
		wantWarning  string
		wantLastDown time.Time
	}{
		{"inflight scales up", Input{Config: inflightOnly, Current: 2, Ready: 2, Inflight: 28, CPU: -1}, 3, "", time.Time{}},
		{"cpu scales up", Input{Config: cpuOnly, Current: 2, Ready: 2, Inflight: -1, CPU: 140}, 4, "", time.Time{}},
		{"higher signal wins", Input{Config: both, Current: 2, Ready: 2, Inflight: 20, CPU: 105}, 3, "", time.Time{}},
		{"clamps to max", Input{Config: inflightOnly, Current: 2, Ready: 2, Inflight: 200, CPU: -1}, 6, "", time.Time{}},
		{"within tolerance holds", Input{Config: inflightOnly, Current: 2, Ready: 2, Inflight: 21, CPU: -1}, 2, "", time.Time{}},
		{"scale down waits for window", Input{Config: inflightOnly, Current: 4, Ready: 4, Inflight: 8, CPU: -1,
			History: []Rec{{At: now.Add(-2 * time.Minute), N: 4}}}, 4, "", time.Time{}},
		{"scale down one step after quiet window", Input{Config: inflightOnly, Current: 4, Ready: 4, Inflight: 8, CPU: -1,
			History: []Rec{{At: now.Add(-6 * time.Minute), N: 4}}}, 3, "", now},
		{"scale down limited per step", Input{Config: inflightOnly, Current: 4, Ready: 4, Inflight: 8, CPU: -1, LastDown: now.Add(-30 * time.Second)}, 4, "", now.Add(-30 * time.Second)},
		{"cpu missing never scales down", Input{Config: both, Current: 4, Ready: 4, Inflight: 8, CPU: -1, CPUErr: "x"}, 4, "cpu signal unavailable: x", time.Time{}},
		{"cpu missing still scales up on inflight", Input{Config: both, Current: 2, Ready: 2, Inflight: 40, CPU: -1, CPUErr: "x"}, 4, "cpu signal unavailable: x", time.Time{}},
		{"cpu-only with missing data holds", Input{Config: cpuOnly, Current: 3, Ready: 3, Inflight: -1, CPU: -1, CPUErr: "x"}, 3, "cpu signal unavailable: x", time.Time{}},
		{"no ready replicas holds", Input{Config: inflightOnly, Current: 3, Ready: 0, Inflight: 50, CPU: -1}, 3, "", time.Time{}},
		{"frozen during rollout", Input{Config: inflightOnly, Current: 2, Ready: 2, Inflight: 200, CPU: -1, Rollout: true}, 2, "", time.Time{}},
		{"restart does not scale down before window", Input{Config: inflightOnly, Current: 4, Ready: 4, Inflight: 8, CPU: -1, Since: now.Add(-time.Minute)}, 4, "", time.Time{}},
		{"new bounds clamp current", Input{Config: model.Autoscale{Min: 1, Max: 3, Inflight: 10}, Current: 5, Ready: 0, Inflight: -1, CPU: -1}, 3, "", time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			in.Now = now
			in.Windows = DefaultWindows
			if in.Since.IsZero() {
				in.Since = now.Add(-time.Hour)
			}
			d := Decide(in)
			if d.Desired != tt.want || d.Warning != tt.wantWarning || !d.LastDown.Equal(tt.wantLastDown) {
				t.Fatalf("Decide = %+v; want desired %d, warning %q, lastDown %v", d, tt.want, tt.wantWarning, tt.wantLastDown)
			}
		})
	}
}

func TestDecideRecordsHistoryAndReason(t *testing.T) {
	d := Decide(Input{Config: both, Current: 2, Ready: 2, Inflight: 28, CPU: 35, Now: now, Since: now.Add(-time.Hour), Windows: DefaultWindows,
		History: []Rec{{At: now.Add(-10 * time.Minute), N: 6}, {At: now.Add(-time.Minute), N: 2}}})
	if len(d.History) != 2 || d.History[0].N != 2 || d.History[1] != (Rec{At: now, N: 3}) {
		t.Fatalf("history = %+v; want the old record pruned and this check's 3 appended", d.History)
	}
	if d.Reason != "inflight 14.0/10, cpu 35/70" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// A hold (rollout, no ready replicas, no data) must count as evidence for
// the current size, or the first low reading after a gap longer than the
// scale-down window removes a replica at once.
func TestHoldsKeepScaleDownWindow(t *testing.T) {
	for _, hold := range []struct {
		name string
		in   Input
	}{
		{"rollout", Input{Rollout: true, Ready: 4, Inflight: 8, CPU: -1}},
		{"no ready replicas", Input{Ready: 0, Inflight: -1, CPU: -1}},
		{"no data", Input{Ready: 4, Inflight: -1, CPU: -1}},
	} {
		t.Run(hold.name, func(t *testing.T) {
			first := hold.in
			first.Config, first.Current, first.Windows = inflightOnly, 4, DefaultWindows
			first.Now, first.Since = now.Add(-time.Minute), now.Add(-time.Hour)
			d1 := Decide(first)
			d2 := Decide(Input{Config: inflightOnly, Current: d1.Desired, Ready: 4, Inflight: 8, CPU: -1, History: d1.History,
				LastDown: d1.LastDown, Since: now.Add(-time.Hour), Now: now, Windows: DefaultWindows})
			if d2.Desired != 4 {
				t.Fatalf("desired %d right after a %s hold; a single low reading must not scale down", d2.Desired, hold.name)
			}
		})
	}
}

func TestDecideHoldReasons(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Input
		want string
	}{
		{"rollout", Input{Rollout: true, Ready: 2, Inflight: 8, CPU: -1}, "frozen during rollout"},
		{"paused rollout", Input{Rollout: true, Paused: true, Ready: 2, Inflight: 8, CPU: -1}, "frozen while the rollout is paused"},
		{"no data", Input{Ready: 2, Inflight: -1, CPU: -1}, "waiting for data"},
	} {
		in := tc.in
		in.Config, in.Current, in.Now, in.Since, in.Windows = inflightOnly, 2, now, now.Add(-time.Hour), DefaultWindows
		if got := Decide(in).Reason; got != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}
