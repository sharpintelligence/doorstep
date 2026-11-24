package metrics

import "testing"

func TestLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"outcome succeeded", OutcomeSucceeded.label(), "succeeded"},
		{"outcome failed", OutcomeFailed.label(), "failed"},
		{"outcome rolled back", OutcomeRolledBack.label(), "rolled_back"},
		{"outcome rollback failed", OutcomeRollbackFailed.label(), "rollback_failed"},
		{"outcome cancelled", OutcomeCancelled.label(), "cancelled"},
		{"outcome superseded", OutcomeSuperseded.label(), "superseded"},
		{"outcome unknown", Outcome("exploded").label(), "unknown"},
		{"outcome empty", Outcome("").label(), "unknown"},
		{"reason gate failed", RollbackReasonGateFailed.label(), "gate_failed"},
		{"reason sync failed", RollbackReasonSyncFailed.label(), "sync_failed"},
		{"reason manual", RollbackReasonManual.label(), "manual"},
		{"reason unknown", RollbackReason("because").label(), "unknown"},
		{"phase deploy", PhaseDeploy.label(), "deploy"},
		{"phase rollback", PhaseRollback.label(), "rollback"},
		{"phase unknown", Phase("later").label(), "unknown"},
		{"result passed", GateResultPassed.label(), "passed"},
		{"result failed", GateResultFailed.label(), "failed"},
		{"result error", GateResultError.label(), "error"},
		{"result unknown", GateResult("maybe").label(), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("label() = %q, want %q", tt.got, tt.want)
			}
		})
	}
}
