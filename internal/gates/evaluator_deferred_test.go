package gates_test

import (
	"slices"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

func deferredBinding(g gates.Gate) gates.Binding {
	b := bind("backup", core.GatePhaseBefore, g)
	b.Deferred = true
	return b
}

func TestEvaluateDefersGateUntilEarlierGatesClear(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		earlier      [][]core.GateStatus
		passes       int
		wantCalls    int
		wantRecorded bool
		wantStatus   core.GateStatus
	}{
		{name: "earlier gate pending", earlier: [][]core.GateStatus{{core.GatePending}}, passes: 3, wantStatus: core.GatePending},
		{name: "earlier gate failed", earlier: [][]core.GateStatus{{core.GateFailed}}, passes: 2, wantStatus: core.GateFailed},
		{name: "earlier gate passes later", earlier: [][]core.GateStatus{{core.GatePending, core.GatePassed}}, passes: 2, wantCalls: 1, wantRecorded: true, wantStatus: core.GatePending},
		{name: "earlier gates passed and skipped", earlier: [][]core.GateStatus{{core.GatePassed}, {core.GateSkipped}}, passes: 1, wantCalls: 1, wantRecorded: true, wantStatus: core.GatePending},
		{name: "one of two earlier gates pending", earlier: [][]core.GateStatus{{core.GatePassed}, {core.GatePending}}, passes: 2, wantStatus: core.GatePending},
		{name: "no earlier gates", passes: 1, wantCalls: 1, wantRecorded: true, wantStatus: core.GatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var bindings []gates.Binding
			for i, statuses := range tt.earlier {
				bindings = append(bindings, bind("gate-"+string(rune('a'+i)), core.GatePhaseBefore, verdicts(statuses...)))
			}
			backup := verdicts(core.GatePending)
			bindings = append(bindings, deferredBinding(backup))
			h := newHarness(t, gates.Plan{Before: bindings})
			d := deployment(core.KindDeploy)

			var out core.GateOutcome
			for range tt.passes {
				out = mustEvaluate(t, h, d, core.GatePhaseBefore)
			}
			if backup.calls() != tt.wantCalls {
				t.Errorf("deferred gate called %d times, want %d", backup.calls(), tt.wantCalls)
			}
			if _, recorded := h.store.get(d.ID, "backup"); recorded != tt.wantRecorded {
				t.Errorf("deferred gate recorded = %v, want %v", recorded, tt.wantRecorded)
			}
			if out.Status != tt.wantStatus {
				t.Errorf("status = %s, want %s (%v)", out.Status, tt.wantStatus, outcomeStatuses(out))
			}
		})
	}
}

func TestEvaluateDeferredTimeoutStartsWhenCalled(t *testing.T) {
	t.Parallel()

	checks := verdicts(core.GatePending, core.GatePending, core.GatePassed)
	deferred := deferredBinding(verdicts(core.GatePending))
	deferred.Timeout = 30 * time.Minute
	h := newHarness(t, gates.Plan{Before: []gates.Binding{bind("checks", core.GatePhaseBefore, checks), deferred}})
	d := deployment(core.KindDeploy)

	mustEvaluate(t, h, d, core.GatePhaseBefore)
	h.clock.Advance(time.Hour)
	mustEvaluate(t, h, d, core.GatePhaseBefore)
	h.clock.Advance(time.Hour)
	out := mustEvaluate(t, h, d, core.GatePhaseBefore)
	if got := outcomeStatuses(out); !slices.Equal(got, []string{"checks=passed", "backup=pending"}) {
		t.Fatalf("results = %v, want the backup pending once checks passed", got)
	}
	h.clock.Advance(29 * time.Minute)
	if out := mustEvaluate(t, h, d, core.GatePhaseBefore); out.Status != core.GatePending {
		t.Fatalf("status after 29m = %s, want pending", out.Status)
	}
	h.clock.Advance(time.Minute)
	out = mustEvaluate(t, h, d, core.GatePhaseBefore)
	if out.Status != core.GateFailed || out.Results[1].Message != "timed out after 30m0s" {
		t.Errorf("outcome = %s %v, want the backup timed out 30m after its first call", out.Status, out.Results)
	}
}

func TestEvaluateDeferredKeepsRecordedResultWhileEarlierGateReruns(t *testing.T) {
	t.Parallel()

	checks := verdicts(core.GatePassed, core.GatePending)
	backup := verdicts(core.GatePassed)
	h := newHarness(t, gates.Plan{Before: []gates.Binding{bind("checks", core.GatePhaseBefore, checks), deferredBinding(backup)}})
	d := deployment(core.KindDeploy)

	mustEvaluate(t, h, d, core.GatePhaseBefore)
	out, err := h.eval.Reevaluate(t.Context(), d, core.GatePhaseBefore)
	if err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	if got := outcomeStatuses(out); !slices.Equal(got, []string{"checks=pending", "backup=passed"}) {
		t.Errorf("results = %v, want the recorded backup result kept", got)
	}
	if backup.calls() != 1 {
		t.Errorf("backup called %d times, want 1", backup.calls())
	}
}
