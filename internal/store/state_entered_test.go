package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestStateEnteredAt(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreateIn(t, st, newDeployment("web"), core.StateSyncing)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = '2026-01-02T03:04:05Z' WHERE deployment_id = $1 AND to_state = 'syncing'`, d.ID)
	mustExec(t, st, `INSERT INTO deployment_events (deployment_id, app_id, type, actor, message, occurred_at)
		SELECT id, app_id, 'note', 'worker', 'later note', '2026-02-01T00:00:00Z' FROM deployments WHERE id = $1`, d.ID)
	cancelled := mustCreateIn(t, st, newDeployment("web"), core.StateCancelled)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = '2026-01-01T00:00:00Z' WHERE deployment_id = $1 AND to_state = 'queued'`, cancelled.ID)

	tests := []struct {
		name  string
		id    core.DeploymentID
		state core.State
		want  time.Time
		err   error
	}{
		{name: "entered state", id: d.ID, state: core.StateSyncing, want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{name: "queued at creation", id: cancelled.ID, state: core.StateQueued, want: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "never entered", id: d.ID, state: core.StateVerifying, err: core.ErrNotFound},
		{name: "skipped state", id: cancelled.ID, state: core.StateResolving, err: core.ErrNotFound},
		{name: "unknown deployment", id: d.ID + cancelled.ID + 1000, state: core.StateSyncing, err: core.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.StateEnteredAt(t.Context(), tt.id, tt.state)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("StateEnteredAt = %v, %v, want %v", got, err, tt.err)
				}
				return
			}
			if err != nil || !got.Equal(tt.want) {
				t.Fatalf("StateEnteredAt = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestStateEnteredAtNewestTransitionWins(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreateIn(t, st, newDeployment("web"), core.StateResolving)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = '2026-01-01T00:00:00Z' WHERE deployment_id = $1`, d.ID)
	mustExec(t, st, `INSERT INTO deployment_events (deployment_id, app_id, type, from_state, to_state, actor, occurred_at)
		SELECT id, app_id, 'transition', 'queued', 'resolving', 'worker', '2026-03-01T00:00:00Z' FROM deployments WHERE id = $1`, d.ID)

	got, err := st.StateEnteredAt(t.Context(), d.ID, core.StateResolving)
	if err != nil || !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("StateEnteredAt = %v, %v, want the newest transition", got, err)
	}
}
