package core

import "testing"

func TestHealthStatusValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status HealthStatus
		want   bool
	}{
		{HealthHealthy, true},
		{HealthProgressing, true},
		{HealthDegraded, true},
		{HealthSuspended, true},
		{HealthMissing, true},
		{HealthUnknown, true},
		{"", false},
		{"Healthy", false},
		{"broken", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			t.Parallel()
			if got := tt.status.Valid(); got != tt.want {
				t.Fatalf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncStateValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state SyncState
		want  bool
	}{
		{SyncSynced, true},
		{SyncOutOfSync, true},
		{SyncUnknown, true},
		{"", false},
		{"Synced", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			t.Parallel()
			if got := tt.state.Valid(); got != tt.want {
				t.Fatalf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncStatusReady(t *testing.T) {
	t.Parallel()

	ready := SyncStatus{Reached: true, State: SyncSynced, Health: Health{Status: HealthHealthy}, OperationPhase: OperationSucceeded}
	tests := []struct {
		name   string
		mutate func(*SyncStatus)
		want   bool
	}{
		{name: "synced, healthy and reached", mutate: func(*SyncStatus) {}, want: true},
		{name: "no operation", mutate: func(s *SyncStatus) { s.OperationPhase = "" }, want: true},
		{name: "terminal failed operation does not block", mutate: func(s *SyncStatus) { s.OperationPhase = OperationFailed }, want: true},
		{name: "not reached", mutate: func(s *SyncStatus) { s.Reached = false }},
		{name: "out of sync", mutate: func(s *SyncStatus) { s.State = SyncOutOfSync }},
		{name: "unknown sync", mutate: func(s *SyncStatus) { s.State = SyncUnknown }},
		{name: "progressing", mutate: func(s *SyncStatus) { s.Health.Status = HealthProgressing }},
		{name: "degraded", mutate: func(s *SyncStatus) { s.Health.Status = HealthDegraded }},
		{name: "operation running", mutate: func(s *SyncStatus) { s.OperationPhase = OperationRunning }},
		{name: "operation terminating", mutate: func(s *SyncStatus) { s.OperationPhase = OperationTerminating }},
		{name: "health older than the operation", mutate: func(s *SyncStatus) { s.HealthStale = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := ready
			tt.mutate(&s)
			if got := s.Ready(); got != tt.want {
				t.Fatalf("Ready() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncFailureReasonClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reason    SyncFailureReason
		automated FailureClass
		manual    FailureClass
	}{
		{SyncReasonNotObserved, FailureTransient, FailureTransient},
		{SyncReasonOutOfSync, FailureUnknown, FailureConfiguration},
		{SyncReasonRunning, FailureUnknown, FailureUnknown},
		{SyncReasonError, FailureTransient, FailureTransient},
		{SyncReasonFailed, FailureUnknown, FailureUnknown},
		{SyncReasonDegraded, FailureVersion, FailureVersion},
		{SyncReasonProgressing, FailureUnknown, FailureUnknown},
		{SyncReasonSuspended, FailureUnknown, FailureUnknown},
		{SyncReasonMissing, FailureUnknown, FailureUnknown},
		{SyncReasonNotFound, FailureConfiguration, FailureConfiguration},
		{SyncReasonForbidden, FailureConfiguration, FailureConfiguration},
		{SyncReasonUnavailable, FailureTransient, FailureTransient},
		{SyncReasonUntracked, FailureConfiguration, FailureConfiguration},
		{"something_new", FailureUnknown, FailureUnknown},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			t.Parallel()
			if got := tt.reason.Class(true); got != tt.automated {
				t.Errorf("Class(automated) = %s, want %s", got, tt.automated)
			}
			if got := tt.reason.Class(false); got != tt.manual {
				t.Errorf("Class(manual) = %s, want %s", got, tt.manual)
			}
		})
	}
}
