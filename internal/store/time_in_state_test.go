package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const measureSlack = time.Minute

func TestTimeInState(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreateIn(t, st, newDeployment("web"), core.StateSyncing)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = now() - interval '90 minutes' WHERE deployment_id = $1 AND to_state = 'syncing'`, d.ID)
	mustExec(t, st, `INSERT INTO deployment_events (deployment_id, app_id, type, actor, message, occurred_at)
		SELECT id, app_id, 'note', 'worker', 'later note', now() FROM deployments WHERE id = $1`, d.ID)
	cancelled := mustCreateIn(t, st, newDeployment("web"), core.StateCancelled)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = now() - interval '3 days' WHERE deployment_id = $1 AND to_state = 'queued'`, cancelled.ID)
	future := mustCreateIn(t, st, newDeployment("api"), core.StateResolving)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = now() + interval '2 minutes' WHERE deployment_id = $1 AND to_state = 'resolving'`, future.ID)

	tests := []struct {
		name  string
		id    core.DeploymentID
		state core.State
		want  time.Duration
		err   error
	}{
		{name: "entered state", id: d.ID, state: core.StateSyncing, want: 90 * time.Minute},
		{name: "queued at creation", id: cancelled.ID, state: core.StateQueued, want: 72 * time.Hour},
		{name: "entered on a clock ahead of the database", id: future.ID, state: core.StateResolving, want: -2 * time.Minute},
		{name: "never entered", id: d.ID, state: core.StateVerifying, err: core.ErrNotFound},
		{name: "skipped state", id: cancelled.ID, state: core.StateResolving, err: core.ErrNotFound},
		{name: "unknown deployment", id: d.ID + cancelled.ID + future.ID + 1000, state: core.StateSyncing, err: core.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.TimeInState(t.Context(), tt.id, tt.state)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("TimeInState = %v, %v, want %v", got, err, tt.err)
				}
				return
			}
			if err != nil || got < tt.want || got > tt.want+measureSlack {
				t.Fatalf("TimeInState = %v, %v, want about %v", got, err, tt.want)
			}
		})
	}
}

func TestTimeInStateNewestTransitionWins(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreateIn(t, st, newDeployment("web"), core.StateResolving)
	mustExec(t, st, `UPDATE deployment_events SET occurred_at = now() - interval '2 hours' WHERE deployment_id = $1`, d.ID)
	mustExec(t, st, `INSERT INTO deployment_events (deployment_id, app_id, type, from_state, to_state, actor, occurred_at)
		SELECT id, app_id, 'transition', 'queued', 'resolving', 'worker', now() - interval '5 minutes' FROM deployments WHERE id = $1`, d.ID)

	got, err := st.TimeInState(t.Context(), d.ID, core.StateResolving)
	if err != nil || got < 5*time.Minute || got > 5*time.Minute+measureSlack {
		t.Fatalf("TimeInState = %v, %v, want the newest transition", got, err)
	}
}
