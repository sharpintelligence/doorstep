package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

type eventRow struct {
	from, to, actor, message string
	data                     map[string]any
}

func lastEvent(t *testing.T, st *store.Store, id core.DeploymentID, typ core.EventType) eventRow {
	t.Helper()
	var e eventRow
	var raw []byte
	err := store.QueryRowSQL(t.Context(), st, `
		SELECT COALESCE(from_state, ''), COALESCE(to_state, ''), actor, message, data
		FROM deployment_events WHERE deployment_id = $1 AND type = $2 ORDER BY id DESC LIMIT 1`, id, typ).
		Scan(&e.from, &e.to, &e.actor, &e.message, &raw)
	if err != nil {
		t.Fatalf("read last %s event of deployment %d: %v", typ, id, err)
	}
	if err := json.Unmarshal(raw, &e.data); err != nil {
		t.Fatalf("event data %q: %v", raw, err)
	}
	return e
}

func countEvents(t *testing.T, st *store.Store, id core.DeploymentID) int {
	t.Helper()
	return queryInt(t, st, "SELECT count(*) FROM deployment_events WHERE deployment_id = $1", id)
}

func queueEntries(t *testing.T, st *store.Store, id core.DeploymentID) int {
	t.Helper()
	return queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1", id)
}

func TestTransitionDeploymentEveryLegalEdge(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for _, from := range core.States() {
		for _, to := range core.States() {
			if !from.CanTransitionTo(to) {
				continue
			}
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				t.Parallel()
				d := mustCreateIn(t, st, newDeployment(string(from)+"-"+string(to)), from)
				events := countEvents(t, st, d.ID)

				got, err := st.TransitionDeployment(t.Context(), store.Transition{
					ID: d.ID, From: from, To: to, Actor: "worker-1", Message: "step",
					Data: json.RawMessage(`{"attempt":2}`),
				})
				if err != nil {
					t.Fatalf("TransitionDeployment: %v", err)
				}
				if got.State != to || got.ID != d.ID {
					t.Errorf("returned deployment %d in %s, want %d in %s", got.ID, got.State, d.ID, to)
				}
				if (got.StartedAt != nil) != (to.Active() || d.StartedAt != nil) {
					t.Errorf("StartedAt = %v (was %v), moving to %s", got.StartedAt, d.StartedAt, to)
				}
				if d.StartedAt != nil && !got.StartedAt.Equal(*d.StartedAt) {
					t.Errorf("StartedAt moved from %v to %v", d.StartedAt, got.StartedAt)
				}
				if (got.FinishedAt != nil) != to.Terminal() {
					t.Errorf("FinishedAt = %v, terminal %v", got.FinishedAt, to.Terminal())
				}
				if got.FinishedAt != nil && got.FinishedAt.Before(got.CreatedAt) {
					t.Errorf("FinishedAt %v before CreatedAt %v", got.FinishedAt, got.CreatedAt)
				}
				if n := queueEntries(t, st, d.ID); n != boolInt(!to.Terminal()) {
					t.Errorf("queue entries = %d, terminal %v", n, to.Terminal())
				}
				if n := countEvents(t, st, d.ID); n != events+1 {
					t.Errorf("events = %d, want %d", n, events+1)
				}
				e := lastEvent(t, st, d.ID, core.EventTransition)
				if e.from != string(from) || e.to != string(to) || e.actor != "worker-1" || e.message != "step" || e.data["attempt"] != float64(2) || len(e.data) != 1 {
					t.Errorf("event = %+v", e)
				}
			})
		}
	}
}

func TestTransitionDeploymentRejectsIllegalEdges(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	for _, from := range core.States() {
		for _, to := range append(core.States(), "paused") {
			if from.CanTransitionTo(to) {
				continue
			}
			_, err := st.TransitionDeployment(t.Context(), store.Transition{ID: d.ID, From: from, To: to})
			if !errors.Is(err, core.ErrInvalidTransition) {
				t.Errorf("%s -> %s error = %v, want ErrInvalidTransition", from, to, err)
			}
		}
	}
	if n := countEvents(t, st, d.ID); n != 1 {
		t.Errorf("events = %d after rejected transitions, want 1", n)
	}
}

func TestTransitionDeploymentRejects(t *testing.T) {
	t.Parallel()

	other := core.DeploymentID(1)
	tests := []struct {
		name   string
		mutate func(*store.Transition)
		want   error
		text   string
	}{
		{name: "failure reason on cancel", mutate: func(tr *store.Transition) { tr.FailureReason = "boom" }, text: "failure reason"},
		{name: "superseded by on cancel", mutate: func(tr *store.Transition) { tr.SupersededByID = &other }, text: "superseded by"},
		{name: "array data", mutate: func(tr *store.Transition) { tr.Data = json.RawMessage(`[1]`) }, text: "JSON object"},
		{name: "null data", mutate: func(tr *store.Transition) { tr.Data = json.RawMessage(`null`) }, text: "JSON object"},
		{name: "malformed data", mutate: func(tr *store.Transition) { tr.Data = json.RawMessage(`{"a":`) }, text: "JSON object"},
		{name: "invalid lineage", mutate: func(tr *store.Transition) { tr.Lineage.Digest = "latest" }, want: core.ErrInvalidDigest},
		{name: "stale from", mutate: func(tr *store.Transition) {
			tr.From = core.StateResolving
			tr.To = core.StatePinning
		}, want: core.ErrStateConflict, text: "is queued, expected resolving"},
		{name: "missing deployment", mutate: func(tr *store.Transition) { tr.ID = 1 << 40 }, want: core.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			tr := store.Transition{ID: d.ID, From: core.StateQueued, To: core.StateCancelled}
			tt.mutate(&tr)
			_, err := st.TransitionDeployment(t.Context(), tr)
			if err == nil {
				t.Fatal("TransitionDeployment succeeded")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
			if tt.text != "" && !containsText(err, tt.text) {
				t.Errorf("error %q does not mention %q", err, tt.text)
			}
			got, err := st.GetDeployment(t.Context(), d.ID)
			if err != nil {
				t.Fatalf("GetDeployment: %v", err)
			}
			if got.State != core.StateQueued || countEvents(t, st, d.ID) != 1 || queueEntries(t, st, d.ID) != 1 {
				t.Errorf("rejected transition changed the deployment: %+v", got)
			}
		})
	}
}

func TestTransitionDeploymentOutcomeFields(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)

	failed := mustCreateIn(t, st, newDeployment("failing"), core.StateVerifying)
	got, err := st.TransitionDeployment(t.Context(), store.Transition{
		ID: failed.ID, From: core.StateVerifying, To: core.StateFailed, Actor: "gates", FailureReason: "health gate failed",
	})
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got.FailureReason != "health gate failed" || got.FinishedAt == nil || got.SupersededByID != nil {
		t.Errorf("failed deployment = %+v", got)
	}

	newer := mustCreate(t, st, newDeployment("replaced"), 0)
	older := mustCreate(t, st, newDeployment("replaced"), 0)
	got, err = st.TransitionDeployment(t.Context(), store.Transition{
		ID: older.ID, From: core.StateQueued, To: core.StateSuperseded, SupersededByID: &newer.ID,
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if got.SupersededByID == nil || *got.SupersededByID != newer.ID || got.FailureReason != "" || got.StartedAt != nil {
		t.Errorf("superseded deployment = %+v", got)
	}

	missing := core.DeploymentID(1 << 40)
	_, err = st.TransitionDeployment(t.Context(), store.Transition{
		ID: newer.ID, From: core.StateQueued, To: core.StateSuperseded, SupersededByID: &missing,
	})
	if err == nil || !containsText(err, "deployments_superseded_by_id_fkey") {
		t.Errorf("superseded by a missing deployment: %v", err)
	}
	if n := queueEntries(t, st, newer.ID); n != 1 {
		t.Errorf("failed supersede removed the queue entry: %d entries", n)
	}
}

func TestTransitionDeploymentKeepsStartedAt(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	if d.StartedAt != nil || d.FinishedAt != nil {
		t.Fatalf("new deployment has timestamps: %+v", d)
	}
	resolving := mustAdvance(t, st, d, core.StateResolving)
	if resolving.StartedAt == nil || resolving.FinishedAt != nil {
		t.Fatalf("resolving deployment timestamps: started %v finished %v", resolving.StartedAt, resolving.FinishedAt)
	}
	done := mustAdvance(t, st, resolving, core.StateSucceeded)
	if !done.StartedAt.Equal(*resolving.StartedAt) || done.FinishedAt == nil || done.FinishedAt.Before(*done.StartedAt) {
		t.Errorf("succeeded deployment timestamps: started %v (was %v) finished %v", done.StartedAt, resolving.StartedAt, done.FinishedAt)
	}
	if !done.UpdatedAt.After(d.UpdatedAt) {
		t.Errorf("UpdatedAt %v not after %v", done.UpdatedAt, d.UpdatedAt)
	}
}

func TestTransitionDeploymentRecordsLineage(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreateIn(t, st, newDeployment("web"), core.StatePinning)
	got, err := st.TransitionDeployment(t.Context(), store.Transition{
		ID: d.ID, From: core.StatePinning, To: core.StateSyncing, Actor: "worker",
		Data:    json.RawMessage(`{"attempt":1,"pin_commit_sha":"overridden"}`),
		Lineage: core.Lineage{GitOpsRepository: "acme/gitops", PinCommitSHA: otherSHA},
	})
	if err != nil {
		t.Fatalf("TransitionDeployment: %v", err)
	}
	if got.GitOpsRepository != "acme/gitops" || got.PinCommitSHA != otherSHA || got.Digest != "" || got.ArgoCDRevision != "" {
		t.Errorf("lineage = %+v", got)
	}
	e := lastEvent(t, st, d.ID, core.EventTransition)
	want := map[string]any{"attempt": float64(1), "gitops_repository": "acme/gitops", "pin_commit_sha": otherSHA}
	if !reflect.DeepEqual(e.data, want) {
		t.Errorf("event data = %v, want %v", e.data, want)
	}
}

func TestTransitionDeploymentConcurrentWorkers(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	const workers = 8
	targets := []core.State{core.StateResolving, core.StateCancelled, core.StateSuperseded}
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Go(func() {
			<-start
			_, errs[i] = st.TransitionDeployment(t.Context(), store.Transition{
				ID: d.ID, From: core.StateQueued, To: targets[i%len(targets)], Actor: fmt.Sprintf("worker-%d", i),
			})
		})
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case !errors.Is(err, core.ErrStateConflict):
			t.Errorf("worker %d error = %v, want ErrStateConflict", i, err)
		}
	}
	if winners != 1 {
		t.Errorf("%d workers won the transition, want 1", winners)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployment_events WHERE deployment_id = $1 AND from_state = 'queued'", d.ID); n != 1 {
		t.Errorf("transition events from queued = %d, want 1", n)
	}
}

func TestTransitionDeploymentComposesInOuterTransaction(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		outcome error
	}{
		{name: "commit"},
		{name: "rollback", outcome: errBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreateIn(t, st, newDeployment("web"), core.StateVerifying)
			err := st.InTx(t.Context(), func(tx *store.Store) error {
				if _, err := tx.TransitionDeployment(t.Context(), store.Transition{
					ID: d.ID, From: core.StateVerifying, To: core.StateFailed, FailureReason: "soak failed",
				}); err != nil {
					return err
				}
				rb := newDeployment("web")
				rb.Kind = core.KindRollback
				rb.Source = core.SourceSystem
				rb.RollbackOfID = &d.ID
				if _, err := tx.CreateDeployment(t.Context(), rb, 100); err != nil {
					return err
				}
				return tt.outcome
			})
			if !errors.Is(err, tt.outcome) {
				t.Fatalf("InTx = %v, want %v", err, tt.outcome)
			}
			got, err := st.GetDeployment(t.Context(), d.ID)
			if err != nil {
				t.Fatalf("GetDeployment: %v", err)
			}
			committed := tt.outcome == nil
			wantState := core.StateVerifying
			if committed {
				wantState = core.StateFailed
			}
			if got.State != wantState {
				t.Errorf("state = %s, want %s", got.State, wantState)
			}
			if n := queueEntries(t, st, d.ID); n != boolInt(!committed) {
				t.Errorf("queue entries = %d, committed %v", n, committed)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM deployments WHERE rollback_of_id = $1", d.ID); n != boolInt(committed) {
				t.Errorf("rollback rows = %d, committed %v", n, committed)
			}
		})
	}
}

func containsText(err error, text string) bool {
	return err != nil && strings.Contains(err.Error(), text)
}
