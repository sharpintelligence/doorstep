package deploys_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
)

var (
	blog    = core.AppTarget("blog")
	alice   = core.Actor{Kind: core.ActorUser, ID: "oidc:alice", Name: "alice"}
	sha1    = strings.Repeat("1", 40)
	sha2    = strings.Repeat("2", 40)
	unknown = strings.Repeat("e", 40)
	blogApp = deploys.App{Target: blog, Repository: "acme/blog", Branch: "main", ImageRepository: "ghcr.io/acme/blog"}
)

type harness struct {
	repo    *fakeRepo
	commits *fakeCommits
	svc     *deploys.Service
}

func newHarness(t *testing.T, migrations fakeMigrations) *harness {
	t.Helper()
	h := &harness{repo: newFakeRepo(), commits: &fakeCommits{onBranch: map[string]bool{sha1: true, sha2: false}}}
	svc, err := deploys.New(deploys.Options{Repository: h.repo, Apps: []deploys.App{blogApp}, Commits: h.commits, Migrations: migrations})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h
}

func detail(t *testing.T, e core.AuditEntry) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(e.Detail, &got); err != nil {
		t.Fatalf("audit detail %s: %v", e.Detail, err)
	}
	return got
}

func TestNewValidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts deploys.Options
		want string
	}{
		{name: "nothing", opts: deploys.Options{}, want: "repository is required"},
		{name: "no commits", opts: deploys.Options{Repository: newFakeRepo(), Migrations: fakeMigrations{}}, want: "commits are required"},
		{name: "no migrations", opts: deploys.Options{Repository: newFakeRepo(), Commits: &fakeCommits{}}, want: "migrations are required"},
		{
			name: "invalid app",
			opts: deploys.Options{Repository: newFakeRepo(), Commits: &fakeCommits{}, Migrations: fakeMigrations{}, Apps: []deploys.App{{Target: blog}}},
			want: "branch must not be empty",
		},
		{
			name: "duplicate app",
			opts: deploys.Options{Repository: newFakeRepo(), Commits: &fakeCommits{}, Migrations: fakeMigrations{}, Apps: []deploys.App{blogApp, blogApp}},
			want: "duplicate target",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := deploys.New(tt.opts); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDeploy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		req          deploys.Request
		migrations   fakeMigrations
		setup        func(h *harness)
		wantErr      error
		wantBlocked  bool
		wantCreated  bool
		wantPriority core.Priority
		wantDetail   map[string]any
		wantChecked  bool
	}{
		{
			name:         "queues a commit on the branch",
			req:          deploys.Request{Target: blog, CommitSHA: sha1, Reason: "  hotfix\n for  checkout ", Actor: alice, Role: core.RoleDeployer},
			wantCreated:  true,
			wantPriority: core.PriorityNormal,
			wantDetail:   map[string]any{"commit": sha1, "urgent": false, "created": true, "reason": "hotfix for checkout"},
			wantChecked:  true,
		},
		{
			name:         "urgent raises the priority",
			req:          deploys.Request{Target: blog, CommitSHA: sha1, Urgent: true, Actor: alice, Role: core.RoleDeployer},
			wantCreated:  true,
			wantPriority: core.PriorityUrgent,
			wantDetail:   map[string]any{"commit": sha1, "urgent": true, "created": true},
			wantChecked:  true,
		},
		{
			name:         "carries the migration flag",
			req:          deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice, Role: core.RoleDeployer},
			migrations:   fakeMigrations{flagged: true, reason: "label db-migration on #4 (1111111)"},
			wantCreated:  true,
			wantPriority: core.PriorityNormal,
			wantDetail:   map[string]any{"commit": sha1, "urgent": false, "created": true, "migration": true},
			wantChecked:  true,
		},
		{
			name: "an active deploy of the commit is returned and raised",
			req:  deploys.Request{Target: blog, CommitSHA: sha1, Urgent: true, Actor: alice, Role: core.RoleDeployer},
			setup: func(h *harness) {
				h.repo.add(core.Deployment{
					Target: blog, Kind: core.KindDeploy, State: core.StateQueued, Source: core.SourceWebhook, RequestedBy: "octocat",
					Commit: core.Commit{Repository: "acme/blog", SHA: sha1}, Image: core.ImageRef{Repository: "ghcr.io/acme/blog", Tag: sha1},
				})
			},
			wantPriority: core.PriorityUrgent,
			wantDetail:   map[string]any{"commit": sha1, "urgent": true, "created": false},
			wantChecked:  true,
		},
		{name: "unknown app", req: deploys.Request{Target: core.AppTarget("shop"), CommitSHA: sha1, Actor: alice}, wantErr: deploys.ErrUnknownApp},
		{name: "short sha", req: deploys.Request{Target: blog, CommitSHA: "abc123", Actor: alice}, wantErr: deploys.ErrInvalidRequest},
		{name: "uppercase sha", req: deploys.Request{Target: blog, CommitSHA: strings.Repeat("A", 40), Actor: alice}, wantErr: deploys.ErrInvalidRequest},
		{name: "no actor", req: deploys.Request{Target: blog, CommitSHA: sha1}, wantErr: deploys.ErrInvalidRequest},
		{name: "unknown role", req: deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice, Role: "root"}, wantErr: deploys.ErrInvalidRequest},
		{name: "reason too long", req: deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice, Reason: strings.Repeat("x", 501)}, wantErr: deploys.ErrInvalidRequest},
		{name: "invalid utf-8 reason", req: deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice, Reason: "\xff"}, wantErr: deploys.ErrInvalidRequest},
		{name: "commit not found", req: deploys.Request{Target: blog, CommitSHA: unknown, Actor: alice}, wantErr: deploys.ErrCommitNotFound, wantChecked: true},
		{name: "commit not on the branch", req: deploys.Request{Target: blog, CommitSHA: sha2, Actor: alice}, wantErr: deploys.ErrCommitNotOnBranch, wantChecked: true},
		{
			name:        "github unavailable",
			req:         deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice},
			setup:       func(h *harness) { h.commits.err = errors.New("502 bad gateway") },
			wantErr:     deploys.ErrUnavailable,
			wantChecked: true,
		},
		{
			name:        "migration detection fails closed",
			req:         deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice},
			migrations:  fakeMigrations{err: errors.New("rate limited")},
			wantErr:     deploys.ErrUnavailable,
			wantChecked: true,
		},
		{
			name: "blocked commit",
			req:  deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice},
			setup: func(h *harness) {
				h.repo.blocks = []core.Block{{ID: 7, Target: blog, Commit: core.Commit{SHA: sha1}, Reason: "broke checkout", CreatedBy: "user:oidc:bob"}}
			},
			wantErr:     core.ErrBlocked,
			wantBlocked: true,
			wantChecked: true,
		},
		{
			name:        "audit failure rolls back the enqueue",
			req:         deploys.Request{Target: blog, CommitSHA: sha1, Actor: alice},
			setup:       func(h *harness) { h.repo.errs["RecordAudit"] = errors.New("disk full") },
			wantErr:     errors.New("disk full"),
			wantChecked: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tt.migrations)
			if tt.setup != nil {
				tt.setup(h)
			}
			before := h.repo.snapshot()
			got, err := h.svc.Deploy(t.Context(), tt.req)
			if checked := len(h.commits.calls) > 0; checked != tt.wantChecked {
				t.Errorf("OnBranch called = %v, want %v", checked, tt.wantChecked)
			}
			if tt.wantErr != nil {
				if err == nil || (!errors.Is(err, tt.wantErr) && !strings.Contains(err.Error(), tt.wantErr.Error())) {
					t.Fatalf("Deploy error = %v, want %v", err, tt.wantErr)
				}
				if _, ok := errors.AsType[*core.BlockedError](err); ok != tt.wantBlocked {
					t.Errorf("BlockedError = %v, want %v", ok, tt.wantBlocked)
				}
				after := h.repo.snapshot()
				if len(after.deployments) != len(before.deployments) || len(after.audit) != 0 {
					t.Fatalf("failed Deploy wrote %d deployments and %d audit rows", len(after.deployments)-len(before.deployments), len(after.audit))
				}
				return
			}
			if err != nil {
				t.Fatalf("Deploy: %v", err)
			}
			if got.Created != tt.wantCreated {
				t.Errorf("Created = %v, want %v", got.Created, tt.wantCreated)
			}
			state := h.repo.snapshot()
			if p := state.priorities[got.Deployment.ID]; p != tt.wantPriority {
				t.Errorf("priority = %s, want %s", p, tt.wantPriority)
			}
			if len(h.repo.locked) != 1 || h.repo.locked[0] != blog {
				t.Errorf("locked = %v, want [%s]", h.repo.locked, blog)
			}
			if len(state.audit) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(state.audit))
			}
			entry := state.audit[0]
			if entry.Action != core.AuditDeployRequest || entry.Actor != alice || entry.Role != tt.req.Role ||
				entry.Target == nil || *entry.Target != blog || entry.DeploymentID == nil || *entry.DeploymentID != got.Deployment.ID {
				t.Errorf("audit entry = %+v", entry)
			}
			gotDetail := detail(t, entry)
			if len(gotDetail) != len(tt.wantDetail) {
				t.Errorf("audit detail = %v, want %v", gotDetail, tt.wantDetail)
			}
			for k, want := range tt.wantDetail {
				if gotDetail[k] != want {
					t.Errorf("audit detail[%s] = %v, want %v", k, gotDetail[k], want)
				}
			}
			if !tt.wantCreated {
				return
			}
			d := got.Deployment
			want := core.Deployment{
				ID: d.ID, Target: blog, Kind: core.KindDeploy, State: core.StateQueued, Source: core.SourceAPI, RequestedBy: alice.String(),
				Commit: core.Commit{Repository: "acme/blog", SHA: sha1}, Image: core.ImageRef{Repository: "ghcr.io/acme/blog", Tag: sha1},
				Migration: tt.migrations.flagged, MigrationReason: tt.migrations.reason,
			}
			if d != want {
				t.Errorf("deployment = %+v, want %+v", d, want)
			}
		})
	}
}

func TestCancel(t *testing.T) {
	t.Parallel()

	pin := strings.Repeat("f", 40)
	deployment := func(state core.State) core.Deployment {
		return core.Deployment{
			Target: blog, Kind: core.KindDeploy, State: state, Source: core.SourceWebhook, RequestedBy: "octocat",
			Commit: core.Commit{Repository: "acme/blog", SHA: sha1}, Image: core.ImageRef{Repository: "ghcr.io/acme/blog", Tag: sha1},
		}
	}
	type testCase struct {
		name       string
		existing   core.Deployment
		reason     string
		conflicts  int
		missing    bool
		wantErr    error
		wantMsg    string
		wantDetail map[string]any
	}
	tests := []testCase{
		{name: "terminal succeeded", existing: deployment(core.StateSucceeded), wantErr: deploys.ErrNotCancellable},
		{name: "terminal cancelled", existing: deployment(core.StateCancelled), wantErr: deploys.ErrNotCancellable},
		{name: "unknown deployment", existing: deployment(core.StateQueued), missing: true, wantErr: core.ErrNotFound},
		{
			name: "conflict then success", existing: deployment(core.StateQueued), conflicts: 2,
			wantMsg:    "cancelled by user:oidc:alice",
			wantDetail: map[string]any{"from_state": "queued"},
		},
		{name: "conflicts exhausted", existing: deployment(core.StateQueued), conflicts: deploys.MaxAttempts, wantErr: core.ErrStateConflict},
		{name: "reason too long", existing: deployment(core.StateQueued), reason: strings.Repeat("y", 501), wantErr: deploys.ErrInvalidRequest},
	}
	for _, state := range core.ClaimableStates() {
		d := deployment(state)
		want := map[string]any{"from_state": string(state), "reason": "bad build"}
		if state.Active() {
			d.PinCommitSHA = pin
			want["pin_commit_sha"] = pin
		}
		tests = append(tests, testCase{
			name: "from " + string(state), existing: d, reason: " bad\tbuild ",
			wantMsg: "cancelled by user:oidc:alice: bad build", wantDetail: want,
		})
	}
	rollbackOf := core.DeploymentID(99)
	rb := deployment(core.StateSyncing)
	rb.Kind, rb.RollbackOfID = core.KindRollback, &rollbackOf
	tests = append(tests, testCase{
		name: "a rollback", existing: rb,
		wantMsg: "cancelled by user:oidc:alice", wantDetail: map[string]any{"from_state": "syncing"},
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, fakeMigrations{})
			d := h.repo.add(tt.existing)
			h.repo.conflicts = tt.conflicts
			id := d.ID
			if tt.missing {
				id = 404
			}
			got, err := h.svc.Cancel(t.Context(), deploys.CancelRequest{ID: id, Reason: tt.reason, Actor: alice, Role: core.RoleDeployer})
			state := h.repo.snapshot()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Cancel error = %v, want %v", err, tt.wantErr)
				}
				if len(state.audit) != 0 || state.deployments[0].State != tt.existing.State {
					t.Fatalf("failed Cancel changed state: %+v", state)
				}
				return
			}
			if err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			if got.ID != d.ID || got.State != core.StateCancelled {
				t.Fatalf("Cancel = %d %s, want %d cancelled", got.ID, got.State, d.ID)
			}
			if len(state.audit) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(state.audit))
			}
			entry := state.audit[0]
			if entry.Action != core.AuditDeployCancel || entry.DeploymentID == nil || *entry.DeploymentID != d.ID || entry.Role != core.RoleDeployer {
				t.Errorf("audit entry = %+v", entry)
			}
			gotDetail := detail(t, entry)
			if len(gotDetail) != len(tt.wantDetail) {
				t.Errorf("audit detail = %v, want %v", gotDetail, tt.wantDetail)
			}
			for k, want := range tt.wantDetail {
				if gotDetail[k] != want {
					t.Errorf("audit detail[%s] = %v, want %v", k, gotDetail[k], want)
				}
			}
		})
	}
}

func TestCancelRetriesAfterTheWorkerMoved(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fakeMigrations{})
	d := h.repo.add(core.Deployment{
		Target: blog, Kind: core.KindDeploy, State: core.StateQueued, Source: core.SourceWebhook, RequestedBy: "octocat",
		Commit: core.Commit{Repository: "acme/blog", SHA: sha1}, Image: core.ImageRef{Repository: "ghcr.io/acme/blog", Tag: sha1},
	})
	moved := false
	h.repo.beforeTransition = func(f *fakeRepo) {
		if !moved {
			moved = true
			f.moveExternally(d.ID, core.StateResolving)
		}
	}
	got, err := h.svc.Cancel(t.Context(), deploys.CancelRequest{ID: d.ID, Actor: alice})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.State != core.StateCancelled {
		t.Fatalf("state = %s, want cancelled", got.State)
	}
	if audit := h.repo.snapshot().audit; len(audit) != 1 || detail(t, audit[0])["from_state"] != "resolving" {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestCancelRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  deploys.CancelRequest
	}{
		{name: "zero id", req: deploys.CancelRequest{Actor: alice}},
		{name: "negative id", req: deploys.CancelRequest{ID: -1, Actor: alice}},
		{name: "no actor", req: deploys.CancelRequest{ID: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, fakeMigrations{})
			if _, err := h.svc.Cancel(t.Context(), tt.req); !errors.Is(err, deploys.ErrInvalidRequest) {
				t.Fatalf("Cancel error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
