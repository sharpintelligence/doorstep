package core

import "testing"

func TestGatePhaseValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		phase GatePhase
		want  bool
	}{
		{GatePhaseBefore, true},
		{GatePhaseAfter, true},
		{"", false},
		{"during", false},
		{"Before", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			t.Parallel()
			if got := tt.phase.Valid(); got != tt.want {
				t.Errorf("GatePhase(%q).Valid() = %v, want %v", tt.phase, got, tt.want)
			}
		})
	}
}

func TestGateStatusTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status GateStatus
		want   bool
	}{
		{GatePending, false},
		{GatePassed, true},
		{GateFailed, true},
		{GateSkipped, true},
		{"", false},
		{"running", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			t.Parallel()
			if got := tt.status.Terminal(); got != tt.want {
				t.Errorf("GateStatus(%q).Terminal() = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestGateStatusValid(t *testing.T) {
	t.Parallel()

	for _, s := range GateStatuses() {
		if !s.Valid() {
			t.Errorf("%q is listed but not valid", s)
		}
	}
	for _, s := range []GateStatus{"", "running", "Passed"} {
		if s.Valid() {
			t.Errorf("%q is valid", s)
		}
	}
}

func TestAggregateGateStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		statuses []GateStatus
		want     GateStatus
	}{
		{name: "no gates", want: GatePassed},
		{name: "all passed", statuses: []GateStatus{GatePassed, GatePassed}, want: GatePassed},
		{name: "skipped counts as passed", statuses: []GateStatus{GateSkipped, GatePassed}, want: GatePassed},
		{name: "only skipped", statuses: []GateStatus{GateSkipped}, want: GatePassed},
		{name: "pending holds", statuses: []GateStatus{GatePassed, GatePending}, want: GatePending},
		{name: "failed wins over pending", statuses: []GateStatus{GatePending, GateFailed}, want: GateFailed},
		{name: "failed wins over passed", statuses: []GateStatus{GateFailed, GatePassed}, want: GateFailed},
		{name: "failed after pending and skipped", statuses: []GateStatus{GateSkipped, GatePending, GateFailed}, want: GateFailed},
		{name: "unknown status holds", statuses: []GateStatus{GatePassed, "running"}, want: GatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			results := make([]GateResult, 0, len(tt.statuses))
			for _, s := range tt.statuses {
				results = append(results, GateResult{Status: s})
			}
			if got := AggregateGateStatus(results); got != tt.want {
				t.Errorf("AggregateGateStatus(%v) = %s, want %s", tt.statuses, got, tt.want)
			}
		})
	}
}
