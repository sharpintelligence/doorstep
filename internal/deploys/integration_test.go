package deploys_test

import (
	"errors"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestPostgresDeployAndCancel(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	svc, err := deploys.New(deploys.Options{
		Repository: deploys.NewRepository(st),
		Apps:       []deploys.App{blogApp},
		Commits:    &fakeCommits{onBranch: map[string]bool{sha1: true}},
		Migrations: fakeMigrations{flagged: true, reason: "trailer Doorstep-Migration in 1111111"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := t.Context()

	got, err := svc.Deploy(ctx, deploys.Request{Target: blog, CommitSHA: sha1, Urgent: true, Reason: "hotfix", Actor: alice, Role: core.RoleDeployer})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	d := got.Deployment
	if !got.Created || d.State != core.StateQueued || d.Source != core.SourceAPI || d.RequestedBy != alice.String() || !d.Migration {
		t.Fatalf("Deploy = %+v", got)
	}
	entries, err := st.QueueEntries(ctx, blog.App)
	if err != nil {
		t.Fatalf("QueueEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Deployment.ID != d.ID || entries[0].Priority != core.PriorityUrgent {
		t.Fatalf("queue = %+v, want deployment %d at urgent priority", entries, d.ID)
	}

	again, err := svc.Deploy(ctx, deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice, Role: core.RoleDeployer})
	if err != nil {
		t.Fatalf("second Deploy: %v", err)
	}
	if again.Created || again.Deployment.ID != d.ID {
		t.Fatalf("second Deploy = %+v, want the existing deployment %d", again, d.ID)
	}

	cancelled, err := svc.Cancel(ctx, deploys.CancelRequest{ID: d.ID, Reason: "wrong commit", Actor: alice, Role: core.RoleDeployer})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.State != core.StateCancelled || cancelled.FinishedAt == nil {
		t.Fatalf("Cancel = %+v", cancelled)
	}
	entries, err = st.QueueEntries(ctx, blog.App)
	if err != nil {
		t.Fatalf("QueueEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("queue after cancel = %+v, want empty", entries)
	}
	history, err := st.GetDeploymentHistory(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDeploymentHistory: %v", err)
	}
	last := history.Events[len(history.Events)-1]
	if last.To != core.StateCancelled || last.Actor != alice.String() || last.Message != "cancelled by user:oidc:alice: wrong commit" {
		t.Fatalf("last event = %+v", last)
	}
	if _, err := svc.Cancel(ctx, deploys.CancelRequest{ID: d.ID, Actor: alice}); !errors.Is(err, deploys.ErrNotCancellable) {
		t.Fatalf("second Cancel error = %v, want ErrNotCancellable", err)
	}

	audit, err := st.ListAudit(ctx, store.AuditFilter{Target: &blog})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var actions []core.AuditAction
	for _, e := range audit {
		actions = append(actions, e.Action)
		if e.DeploymentID == nil || *e.DeploymentID != d.ID || e.Actor.String() != alice.String() {
			t.Errorf("audit entry = %+v", e)
		}
	}
	want := []core.AuditAction{core.AuditDeployCancel, core.AuditDeployRequest, core.AuditDeployRequest}
	if len(actions) != len(want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("audit actions = %v, want %v", actions, want)
		}
	}
}

func TestPostgresDeployRefusesBlockedCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if _, _, err := st.CreateBlock(t.Context(), core.Block{Target: blog, Commit: core.Commit{SHA: sha1}, Reason: "broke checkout", CreatedBy: "user:oidc:bob"}); err != nil {
		t.Fatalf("CreateBlock: %v", err)
	}
	svc, err := deploys.New(deploys.Options{
		Repository: deploys.NewRepository(st),
		Apps:       []deploys.App{blogApp},
		Commits:    &fakeCommits{onBranch: map[string]bool{sha1: true}},
		Migrations: fakeMigrations{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = svc.Deploy(t.Context(), deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice})
	if _, ok := errors.AsType[*core.BlockedError](err); !ok {
		t.Fatalf("Deploy error = %v, want BlockedError", err)
	}
	page, err := st.ListDeployments(t.Context(), store.DeploymentFilter{App: blog.App})
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("deployments = %+v, want none", page.Items)
	}
}
