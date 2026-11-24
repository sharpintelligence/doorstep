package core

import (
	"testing"
	"time"
)

func TestPriorityString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		priority Priority
		want     string
	}{
		{PriorityNormal, "normal"},
		{PriorityUrgent, "urgent"},
		{PriorityRollback, "rollback"},
		{Priority(5), "5"},
		{Priority(-1), "-1"},
	}
	for _, tt := range tests {
		if got := tt.priority.String(); got != tt.want {
			t.Errorf("Priority(%d).String() = %q, want %q", int(tt.priority), got, tt.want)
		}
	}
}

func TestPrioritiesAreOrdered(t *testing.T) {
	t.Parallel()

	priorities := Priorities()
	for i := 1; i < len(priorities); i++ {
		if priorities[i] <= priorities[i-1] {
			t.Errorf("Priorities()[%d] = %d is not above %d", i, priorities[i], priorities[i-1])
		}
	}
	if PriorityNormal >= PriorityUrgent || PriorityUrgent >= PriorityRollback {
		t.Errorf("want normal < urgent < rollback, got %d, %d, %d", PriorityNormal, PriorityUrgent, PriorityRollback)
	}
}

func TestQueueEntryInFlight(t *testing.T) {
	t.Parallel()

	lease := time.Now().Add(time.Minute)
	tests := []struct {
		name  string
		state State
		lease *time.Time
		want  bool
	}{
		{name: "queued without lease", state: StateQueued, want: false},
		{name: "queued with live lease", state: StateQueued, lease: &lease, want: true},
		{name: "resolving", state: StateResolving, want: true},
		{name: "pinning", state: StatePinning, want: true},
		{name: "syncing", state: StateSyncing, want: true},
		{name: "verifying with lease", state: StateVerifying, lease: &lease, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := QueueEntry{Deployment: Deployment{State: tt.state}, LeaseExpiresAt: tt.lease}
			if got := e.InFlight(); got != tt.want {
				t.Errorf("InFlight() = %v, want %v", got, tt.want)
			}
		})
	}
}
