package rollback_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func pgDeployment(sha string) core.Deployment {
	return core.Deployment{
		Target:      blog,
		Kind:        core.KindDeploy,
		Source:      core.SourceWebhook,
		RequestedBy: "octocat",
		Commit:      core.Commit{Repository: "acme/blog", SHA: sha},
		Image:       core.ImageRef{Repository: "ghcr.io/acme/blog", Tag: sha},
	}
}

func advance(t *testing.T, st *store.Store, d core.Deployment, path ...core.State) core.Deployment {
	t.Helper()
	for _, next := range path {
		tr := store.Transition{ID: d.ID, From: d.State, To: next, Actor: "worker"}
		if next == core.StateResolving {
			tr.Lineage.Digest = d.Digest
		}
		var err error
		d, err = st.TransitionDeployment(t.Context(), tr)
		if err != nil {
			t.Fatalf("transition %d to %s: %v", d.ID, next, err)
		}
	}
	return d
}

func pgDeployed(t *testing.T, st *store.Store, sha string, digest core.Digest, path ...core.State) core.Deployment {
	t.Helper()
	d, _, err := st.EnqueueDeployment(t.Context(), pgDeployment(sha), core.PriorityNormal)
	if err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	d.Digest = digest
	return advance(t, st, d, path...)
}

var toSucceeded = []core.State{core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying, core.StateSucceeded}

func TestPostgresRollback(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	svc, err := rollback.New(rollback.Options{Repository: rollback.NewRepository(st)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pgDeployed(t, st, sha1, digestA, toSucceeded...)
	v2 := pgDeployed(t, st, sha2, digestB, toSucceeded...)
	bad := pgDeployed(t, st, sha3, digestC, core.StateResolving, core.StatePinning, core.StateSyncing)
	sameCommit, err := st.CreateDeployment(t.Context(), pgDeployment(sha3), int(core.PriorityNormal))
	if err != nil {
		t.Fatalf("create second deploy of %s: %v", sha3, err)
	}
	newer := pgDeployed(t, st, sha4, "")

	got, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, Role: core.RoleDeployer, Reason: "5xx spike"})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got.RolledBack.ID != bad.ID || got.RolledBack.State != core.StateCancelled || got.To.ID != v2.ID {
		t.Fatalf("Rollback = rolled back %d (%s) to %d, want %d cancelled to %d", got.RolledBack.ID, got.RolledBack.State, got.To.ID, bad.ID, v2.ID)
	}
	if !slices.Equal(got.Cancelled, []core.DeploymentID{bad.ID, sameCommit.ID}) {
		t.Errorf("Cancelled = %v, want [%d %d]", got.Cancelled, bad.ID, sameCommit.ID)
	}

	entries, err := st.QueueEntries(t.Context(), blog.App)
	if err != nil {
		t.Fatalf("QueueEntries: %v", err)
	}
	if len(entries) != 2 || entries[0].Deployment.ID != got.Rollback.ID || entries[0].Priority != core.PriorityRollback ||
		entries[1].Deployment.ID != newer.ID {
		t.Fatalf("queue = %+v, want the rollback at priority 200 ahead of deploy %d", entries, newer.ID)
	}
	rb := entries[0].Deployment
	if rb.Kind != core.KindRollback || rb.Digest != digestB || rb.Commit.SHA != sha2 || rb.RollbackOfID == nil || *rb.RollbackOfID != bad.ID {
		t.Errorf("rollback deployment = %+v", rb)
	}

	if got.Block == nil || got.Block.Digest != digestC || got.Block.Commit.SHA != sha3 || got.Block.Reason != "5xx spike" ||
		got.Block.RollbackID == nil || *got.Block.RollbackID != rb.ID {
		t.Fatalf("Block = %+v", got.Block)
	}
	audit, err := st.ListAudit(t.Context(), store.AuditFilter{Action: core.AuditRollbackRequest})
	if err != nil || len(audit) != 1 || *audit[0].DeploymentID != rb.ID || audit[0].Actor != alice {
		t.Fatalf("audit = %+v, %v", audit, err)
	}

	if _, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice}); !errors.Is(err, core.ErrRollbackInProgress) {
		t.Fatalf("second Rollback error = %v, want ErrRollbackInProgress", err)
	}

	trailers, err := svc.PendingTrailers(t.Context(), blog)
	if err != nil || !slices.Equal(trailers.Blocks, []core.Digest{digestC}) || len(trailers.Unblocks) != 0 {
		t.Fatalf("PendingTrailers = %+v, %v", trailers, err)
	}
	pin := core.Commit{Repository: "acme/gitops", SHA: sha5}
	if err := svc.MarkMirrored(t.Context(), blog, trailers, pin); err != nil {
		t.Fatalf("MarkMirrored: %v", err)
	}
	if trailers, err := svc.PendingTrailers(t.Context(), blog); err != nil || !trailers.IsZero() {
		t.Fatalf("PendingTrailers after mirroring = %+v, %v", trailers, err)
	}

	blocked, found, err := svc.Blocked(t.Context(), blog, core.Commit{Repository: "acme/blog", SHA: sha3}, "")
	if err != nil || !found || blocked.ID != got.Block.ID {
		t.Fatalf("Blocked(commit) = %+v, %v, %v", blocked, found, err)
	}

	cleared, err := svc.Unblock(t.Context(), blog, core.BlockRef{Digest: digestC}, alice, core.RoleDeployer)
	if err != nil || len(cleared) != 1 {
		t.Fatalf("Unblock = %+v, %v", cleared, err)
	}
	trailers, err = svc.PendingTrailers(t.Context(), blog)
	if err != nil || !slices.Equal(trailers.Unblocks, []core.Digest{digestC}) || len(trailers.Blocks) != 0 {
		t.Fatalf("PendingTrailers after unblock = %+v, %v", trailers, err)
	}
}

func TestPostgresRollbackIsAtomic(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	pgDeployed(t, st, sha1, digestA, toSucceeded...)
	pgDeployed(t, st, sha2, digestB, core.StateResolving, core.StatePinning)
	svc, err := rollback.New(rollback.Options{Repository: failingAudit{rollback.NewRepository(st)}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice}); !errors.Is(err, errBoom) {
		t.Fatalf("Rollback error = %v, want %v", err, errBoom)
	}
	page, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: blog.App})
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].State != core.StatePinning {
		t.Fatalf("deployments = %+v, want the in-flight deploy untouched and no rollback", page.Items)
	}
	blocks, err := st.ListBlocks(t.Context(), blog, true)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("blocks = %+v, %v, want none", blocks, err)
	}
}

type failingAudit struct {
	rollback.Repository
}

func (r failingAudit) InTx(ctx context.Context, fn func(rollback.Repository) error) error {
	return r.Repository.InTx(ctx, func(tx rollback.Repository) error {
		return fn(failingAudit{tx})
	})
}

func (failingAudit) RecordAudit(context.Context, core.AuditEntry) error {
	return errBoom
}

type queueOver struct {
	tx *store.Store
}

func (q queueOver) Enqueue(ctx context.Context, d core.Deployment) (core.Deployment, error) {
	created, _, err := q.tx.EnqueueDeployment(ctx, d, core.PriorityNormal)
	return created, err
}

type staticChecks struct{}

func (staticChecks) Checks(context.Context, string, string, []string) (intake.Snapshot, error) {
	return intake.Snapshot{CheckRuns: []github.CheckRun{{ID: 1, Name: "test", Status: "completed", Conclusion: "success"}}}, nil
}

type staticHead string

func (h staticHead) BranchHead(context.Context, string, string) (string, error) {
	return string(h), nil
}

func TestPostgresIntakeRefusesRolledBackCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	svc, err := rollback.New(rollback.Options{Repository: rollback.NewRepository(st)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pgDeployed(t, st, sha1, digestA, toSucceeded...)
	pgDeployed(t, st, sha2, digestB, toSucceeded...)
	if _, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	in, err := intake.New(intake.Options{
		Apps: []intake.App{{
			Target: blog, Repository: "acme/blog", Branch: "main", ImageRepository: "ghcr.io/acme/blog",
		}},
		Commits:  intake.NewPostgresCommits(st, func(tx *store.Store) intake.Queue { return queueOver{tx} }),
		Checks:   staticChecks{},
		Branches: staticHead(sha2),
	})
	if err != nil {
		t.Fatalf("intake.New: %v", err)
	}
	push := intake.Push{Repository: "acme/blog", Branch: "main", SHA: sha2, Source: core.SourcePoll, PushedBy: "octocat"}
	results, err := in.HandlePush(t.Context(), push)
	if err != nil || len(results) != 1 || results[0].Enqueued || results[0].Blocked == nil {
		t.Fatalf("HandlePush = %+v, %v, want the rolled back commit refused", results, err)
	}

	if _, err := svc.Unblock(t.Context(), blog, core.BlockRef{CommitSHA: sha2}, alice, core.RoleDeployer); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	results, err = in.Reevaluate(t.Context(), "acme/blog", sha2)
	if err != nil || len(results) != 1 || !results[0].Enqueued {
		t.Fatalf("Reevaluate after unblock = %+v, %v, want it enqueued", results, err)
	}
}
