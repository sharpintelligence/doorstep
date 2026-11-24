package rollback_test

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

var toVerifying = []core.State{core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying}

func pgFail(bad core.Deployment, from core.State) func(rollback.Plan) (store.Transition, error) {
	return func(p rollback.Plan) (store.Transition, error) {
		data := map[string]any{"stage": "verifying", "auto_rollback": map[string]any{"outcome": p.Outcome}}
		if p.Rollback != nil {
			data["auto_rollback"] = map[string]any{"outcome": p.Outcome, "rollback_id": p.Rollback.ID}
		}
		encoded, err := json.Marshal(data)
		return store.Transition{
			ID: bad.ID, From: from, To: core.StateFailed, Actor: "doorstep",
			Message: "failed while verifying", Data: encoded, FailureReason: "after gates failed: e2e",
		}, err
	}
}

func newPostgresService(t *testing.T, st *store.Store) *rollback.Service {
	t.Helper()
	svc, err := rollback.New(rollback.Options{Repository: rollback.NewRepository(st)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestPostgresAutomaticRollback(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	svc := newPostgresService(t, st)
	pgDeployed(t, st, sha1, digestA, toSucceeded...)
	v2 := pgDeployed(t, st, sha2, digestB, toSucceeded...)
	bad := pgDeployed(t, st, sha3, digestC, toVerifying...)
	sameCommit, err := st.CreateDeployment(t.Context(), pgDeployment(sha3), int(core.PriorityNormal))
	if err != nil {
		t.Fatalf("create second deploy of %s: %v", sha3, err)
	}
	newer := pgDeployed(t, st, sha4, "")

	got, err := svc.Automatic(t.Context(), automaticRequest(bad, pgFail(bad, bad.State)))
	if err != nil {
		t.Fatalf("Automatic: %v", err)
	}
	if got.Plan.Outcome != rollback.OutcomeQueued || got.Plan.To == nil || got.Plan.To.ID != v2.ID {
		t.Fatalf("plan = %+v, want queued to %d", got.Plan, v2.ID)
	}
	if !slices.Equal(got.Cancelled, []core.DeploymentID{sameCommit.ID}) {
		t.Errorf("Cancelled = %v, want [%d]", got.Cancelled, sameCommit.ID)
	}

	entries, err := st.QueueEntries(t.Context(), blog.App)
	if err != nil {
		t.Fatalf("QueueEntries: %v", err)
	}
	if len(entries) != 2 || entries[0].Deployment.ID != got.Plan.Rollback.ID || entries[0].Priority != core.PriorityRollback ||
		entries[1].Deployment.ID != newer.ID {
		t.Fatalf("queue = %+v, want the rollback at priority 200 ahead of deploy %d", entries, newer.ID)
	}
	rb := entries[0].Deployment
	if rb.Kind != core.KindRollback || rb.Source != core.SourceSystem || rb.RequestedBy != "system:doorstep" ||
		rb.Digest != digestB || rb.Commit.SHA != sha2 || rb.RollbackOfID == nil || *rb.RollbackOfID != bad.ID {
		t.Errorf("rollback deployment = %+v", rb)
	}

	history, err := st.GetDeploymentHistory(t.Context(), bad.ID)
	if err != nil {
		t.Fatalf("GetDeploymentHistory: %v", err)
	}
	if history.Deployment.State != core.StateFailed || history.Deployment.FailureReason != "after gates failed: e2e" {
		t.Errorf("failing deployment = %s %q", history.Deployment.State, history.Deployment.FailureReason)
	}
	last := history.Events[len(history.Events)-1]
	var data struct {
		AutoRollback struct {
			Outcome    rollback.Outcome  `json:"outcome"`
			RollbackID core.DeploymentID `json:"rollback_id"`
		} `json:"auto_rollback"`
	}
	if err := json.Unmarshal(last.Data, &data); err != nil || last.To != core.StateFailed ||
		data.AutoRollback.Outcome != rollback.OutcomeQueued || data.AutoRollback.RollbackID != rb.ID {
		t.Errorf("failed event = %+v (%s), %v", last, last.Data, err)
	}
	if len(history.Rollbacks) != 1 || history.Rollbacks[0].ID != rb.ID {
		t.Errorf("rollbacks of the failing deployment = %+v", history.Rollbacks)
	}

	if b := got.Block; b == nil || b.Digest != digestC || b.Commit.SHA != sha3 || b.Reason != autoReason ||
		b.CreatedBy != "system:doorstep" || b.RollbackID == nil || *b.RollbackID != rb.ID {
		t.Fatalf("Block = %+v", got.Block)
	}
	audit, err := st.ListAudit(t.Context(), store.AuditFilter{Action: core.AuditRollbackRequest})
	if err != nil || len(audit) != 1 || *audit[0].DeploymentID != rb.ID || audit[0].Actor != doorstep || audit[0].Role != "" {
		t.Fatalf("audit = %+v, %v", audit, err)
	}

	if _, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice}); !errors.Is(err, core.ErrRollbackInProgress) {
		t.Fatalf("manual Rollback after the automatic one = %v, want ErrRollbackInProgress", err)
	}
}

func TestPostgresAutomaticRollbackIsAtomic(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	svc := newPostgresService(t, st)
	pgDeployed(t, st, sha1, digestA, toSucceeded...)
	bad := pgDeployed(t, st, sha2, digestB, toVerifying...)

	_, err := svc.Automatic(t.Context(), automaticRequest(bad, pgFail(bad, core.StateSyncing)))
	if !errors.Is(err, core.ErrStateConflict) {
		t.Fatalf("Automatic = %v, want ErrStateConflict", err)
	}
	page, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: blog.App})
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != bad.ID || page.Items[0].State != core.StateVerifying {
		t.Fatalf("deployments = %+v, want the failing deploy untouched and no rollback", page.Items)
	}
	blocks, err := st.ListBlocks(t.Context(), blog, true)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("blocks = %+v, %v, want none", blocks, err)
	}
	audit, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil || len(audit) != 0 {
		t.Fatalf("audit = %+v, %v, want none", audit, err)
	}
}

func TestPostgresAutomaticWithheldForMigrations(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	svc := newPostgresService(t, st)
	pgDeployed(t, st, sha1, digestA, toSucceeded...)
	flagged := pgDeployment(sha2)
	flagged.Migration, flagged.MigrationReason = true, "label db-migration on #12"
	bad, _, err := st.EnqueueDeployment(t.Context(), flagged, core.PriorityNormal)
	if err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	bad.Digest = digestB
	bad = advance(t, st, bad, toVerifying...)

	got, err := svc.Automatic(t.Context(), automaticRequest(bad, pgFail(bad, bad.State)))
	if err != nil {
		t.Fatalf("Automatic: %v", err)
	}
	if got.Plan.Outcome != rollback.OutcomeWithheldMigrations || len(got.Plan.Migrations) != 1 || got.Plan.Migrations[0].ID != bad.ID {
		t.Fatalf("plan = %+v, want withheld naming deployment %d", got.Plan, bad.ID)
	}
	page, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: blog.App, Kinds: []core.Kind{core.KindRollback}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("rollbacks = %+v, %v, want none", page.Items, err)
	}
	if d, err := st.GetDeployment(t.Context(), bad.ID); err != nil || d.State != core.StateFailed {
		t.Fatalf("failing deployment = %s, %v, want failed", d.State, err)
	}
	blocks, err := st.ListBlocks(t.Context(), blog, true)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("blocks = %+v, %v, want none", blocks, err)
	}
}

func TestPostgresAutomaticAndManualRollbackRace(t *testing.T) {
	t.Parallel()

	for range 3 {
		st := storetest.Open(t)
		svc := newPostgresService(t, st)
		pgDeployed(t, st, sha1, digestA, toSucceeded...)
		bad := pgDeployed(t, st, sha2, digestB, toVerifying...)

		var (
			manualErr, autoErr error
			auto               rollback.AutomaticResult
			wg                 sync.WaitGroup
		)
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			_, manualErr = svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, Role: core.RoleDeployer})
		})
		wg.Go(func() {
			<-start
			auto, autoErr = svc.Automatic(t.Context(), automaticRequest(bad, pgFail(bad, bad.State)))
		})
		close(start)
		wg.Wait()

		final, err := st.GetDeployment(t.Context(), bad.ID)
		if err != nil {
			t.Fatalf("GetDeployment: %v", err)
		}
		switch {
		case manualErr == nil && errors.Is(autoErr, core.ErrStateConflict):
			if final.State != core.StateCancelled {
				t.Errorf("manual rollback won but the deploy is %s, want cancelled", final.State)
			}
		case autoErr == nil && errors.Is(manualErr, core.ErrRollbackInProgress):
			if final.State != core.StateFailed || auto.Plan.Outcome != rollback.OutcomeQueued {
				t.Errorf("automatic rollback won but the deploy is %s with outcome %s", final.State, auto.Plan.Outcome)
			}
		default:
			t.Fatalf("manual = %v, automatic = %v, want exactly one to succeed", manualErr, autoErr)
		}
		page, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: blog.App, Kinds: []core.Kind{core.KindRollback}})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("rollback deployments = %+v, %v, want exactly one", page.Items, err)
		}
		blocks, err := st.ListBlocks(t.Context(), blog, false)
		if err != nil || len(blocks) != 1 || blocks[0].Digest != digestB {
			t.Fatalf("blocks = %+v, %v, want one block of %s", blocks, err, digestB)
		}
	}
}
