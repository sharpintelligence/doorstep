package rollback_test

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
)

var (
	blog  = core.Target{App: "blog", Environment: "prod"}
	web   = core.Target{App: "web", Environment: "prod"}
	alice = core.Actor{Kind: core.ActorUser, ID: "alice"}

	digestA = digestOf('a')
	digestB = digestOf('b')
	digestC = digestOf('c')
	digestD = digestOf('d')

	sha1 = shaOf('1')
	sha2 = shaOf('2')
	sha3 = shaOf('3')
	sha4 = shaOf('4')
	sha5 = shaOf('5')
)

func digestOf(c byte) core.Digest {
	return core.Digest("sha256:" + strings.Repeat(string(c), 64))
}

func shaOf(c byte) string {
	return strings.Repeat(string(c), 40)
}

func deployment(t core.Target, state core.State, sha string, digest core.Digest) core.Deployment {
	return core.Deployment{Target: t, State: state, Commit: core.Commit{SHA: sha}, Digest: digest}
}

type history struct {
	v1, v2, v3 core.Deployment
}

func seed(f *fakeRepo) history {
	return history{
		v1: f.add(deployment(blog, core.StateSucceeded, sha1, digestA)),
		v2: f.add(deployment(blog, core.StateSucceeded, sha2, digestB)),
		v3: f.add(deployment(blog, core.StateSucceeded, sha3, digestC)),
	}
}

func newService(t *testing.T, f *fakeRepo) (*rollback.Service, *recordingObserver) {
	t.Helper()
	obs := &recordingObserver{}
	svc, err := rollback.New(rollback.Options{Repository: f, Observer: obs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, obs
}

func idPtr(id core.DeploymentID) *core.DeploymentID {
	return &id
}

func TestNewRequiresRepository(t *testing.T) {
	t.Parallel()

	if _, err := rollback.New(rollback.Options{}); err == nil {
		t.Fatal("New without a repository succeeded")
	}
	if _, err := rollback.New(rollback.Options{Repository: newFakeRepo()}); err != nil {
		t.Fatalf("New without observer or logger: %v", err)
	}
}

func TestRollback(t *testing.T) {
	t.Parallel()

	type want struct {
		rolledBack    func(h history, extra []core.Deployment) core.DeploymentID
		to            func(h history, extra []core.Deployment) core.DeploymentID
		blockDigest   core.Digest
		blockCommit   string
		noBlock       bool
		cancelled     func(h history, extra []core.Deployment) []core.DeploymentID
		stillQueued   func(h history, extra []core.Deployment) []core.DeploymentID
		rolledBackEnd core.State
	}
	v1 := func(h history, _ []core.Deployment) core.DeploymentID { return h.v1.ID }
	v2 := func(h history, _ []core.Deployment) core.DeploymentID { return h.v2.ID }
	v3 := func(h history, _ []core.Deployment) core.DeploymentID { return h.v3.ID }
	extra := func(i int) func(history, []core.Deployment) core.DeploymentID {
		return func(_ history, e []core.Deployment) core.DeploymentID { return e[i].ID }
	}

	tests := []struct {
		name    string
		setup   func(f *fakeRepo, h history) []core.Deployment
		request func(h history, extra []core.Deployment) rollback.Request
		want    want
		wantErr error
	}{
		{
			name: "default target is the previous success",
			want: want{rolledBack: v3, to: v2, blockDigest: digestC, blockCommit: sha3, rolledBackEnd: core.StateSucceeded},
		},
		{
			name: "default target skips blocked digests",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				f.addBlock(core.Block{Target: blog, Digest: digestB, CreatedBy: "user:bob"})
				return nil
			},
			want: want{rolledBack: v3, to: v1, blockDigest: digestC, blockCommit: sha3, rolledBackEnd: core.StateSucceeded},
		},
		{
			name: "explicit deployment",
			request: func(h history, _ []core.Deployment) rollback.Request {
				return rollback.Request{ToDeployment: &h.v1.ID}
			},
			want: want{rolledBack: v3, to: v1, blockDigest: digestC, blockCommit: sha3, rolledBackEnd: core.StateSucceeded},
		},
		{
			name: "explicit digest picks the newest success of it",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{
					f.add(deployment(blog, core.StateSucceeded, sha1, digestA)),
					f.add(deployment(blog, core.StateSucceeded, sha3, digestC)),
				}
			},
			request: func(history, []core.Deployment) rollback.Request { return rollback.Request{ToDigest: digestA} },
			want:    want{rolledBack: extra(1), to: extra(0), blockDigest: digestC, blockCommit: sha3, rolledBackEnd: core.StateSucceeded},
		},
		{
			name: "in-flight deploy is cancelled",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateSyncing, sha4, digestD))}
			},
			want: want{
				rolledBack: extra(0), to: v3, blockDigest: digestD, blockCommit: sha4, rolledBackEnd: core.StateCancelled,
				cancelled: func(_ history, e []core.Deployment) []core.DeploymentID { return []core.DeploymentID{e[0].ID} },
			},
		},
		{
			name: "queued deploys of the blocked commit are cancelled, others stay",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{
					f.add(deployment(blog, core.StateVerifying, sha4, digestD)),
					f.add(deployment(blog, core.StateQueued, sha4, "")),
					f.add(deployment(blog, core.StateQueued, sha5, "")),
					f.add(deployment(web, core.StateQueued, sha4, "")),
				}
			},
			want: want{
				rolledBack: extra(0), to: v3, blockDigest: digestD, blockCommit: sha4, rolledBackEnd: core.StateCancelled,
				cancelled: func(_ history, e []core.Deployment) []core.DeploymentID {
					return []core.DeploymentID{e[0].ID, e[1].ID}
				},
				stillQueued: func(_ history, e []core.Deployment) []core.DeploymentID {
					return []core.DeploymentID{e[2].ID, e[3].ID}
				},
			},
		},
		{
			name: "deploy cancelled before resolving gets a commit-only block",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateResolving, sha4, ""))}
			},
			want: want{
				rolledBack: extra(0), to: v3, blockCommit: sha4, rolledBackEnd: core.StateCancelled,
				cancelled: func(_ history, e []core.Deployment) []core.DeploymentID { return []core.DeploymentID{e[0].ID} },
			},
		},
		{
			name: "commit is not blocked when the target was built from it",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateSucceeded, sha3, digestD))}
			},
			want: want{rolledBack: extra(0), to: v3, blockDigest: digestD, rolledBackEnd: core.StateSucceeded},
		},
		{
			name: "rebuild of the target commit cancelled before resolving blocks nothing",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateResolving, sha3, ""))}
			},
			want: want{
				rolledBack: extra(0), to: v3, noBlock: true, rolledBackEnd: core.StateCancelled,
				cancelled: func(_ history, e []core.Deployment) []core.DeploymentID { return []core.DeploymentID{e[0].ID} },
			},
		},
		{
			name: "pinned failure is the current version",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				d := deployment(blog, core.StateFailed, sha4, digestD)
				d.PinCommitSHA = shaOf('9')
				return []core.Deployment{f.add(d), f.add(deployment(blog, core.StateFailed, sha5, ""))}
			},
			want: want{rolledBack: extra(0), to: v3, blockDigest: digestD, blockCommit: sha4, rolledBackEnd: core.StateFailed},
		},
		{
			name: "unpinned failures and queued deploys are not current",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{
					f.add(deployment(blog, core.StateFailed, sha4, "")),
					f.add(deployment(blog, core.StateQueued, sha5, "")),
				}
			},
			want: want{
				rolledBack: v3, to: v2, blockDigest: digestC, blockCommit: sha3, rolledBackEnd: core.StateSucceeded,
				stillQueued: func(_ history, e []core.Deployment) []core.DeploymentID { return []core.DeploymentID{e[1].ID} },
			},
		},
		{
			name: "explicit deployment of another app",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(web, core.StateSucceeded, sha1, digestA))}
			},
			request: func(_ history, e []core.Deployment) rollback.Request { return rollback.Request{ToDeployment: &e[0].ID} },
			wantErr: core.ErrInvalidRollback,
		},
		{
			name: "explicit deployment that did not succeed",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateCancelled, sha1, digestA)), f.add(deployment(blog, core.StateSucceeded, sha3, digestC))}
			},
			request: func(_ history, e []core.Deployment) rollback.Request { return rollback.Request{ToDeployment: &e[0].ID} },
			wantErr: core.ErrInvalidRollback,
		},
		{
			name: "explicit deployment without a digest",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateSucceeded, sha1, "")), f.add(deployment(blog, core.StateSucceeded, sha3, digestC))}
			},
			request: func(_ history, e []core.Deployment) rollback.Request { return rollback.Request{ToDeployment: &e[0].ID} },
			wantErr: core.ErrInvalidRollback,
		},
		{
			name:    "explicit deployment that is current",
			request: func(h history, _ []core.Deployment) rollback.Request { return rollback.Request{ToDeployment: &h.v3.ID} },
			wantErr: core.ErrInvalidRollback,
		},
		{
			name: "explicit deployment with the current digest",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateSucceeded, sha4, digestB))}
			},
			request: func(h history, _ []core.Deployment) rollback.Request { return rollback.Request{ToDeployment: &h.v2.ID} },
			wantErr: core.ErrInvalidRollback,
		},
		{
			name:    "explicit deployment that does not exist",
			request: func(history, []core.Deployment) rollback.Request { return rollback.Request{ToDeployment: idPtr(999)} },
			wantErr: core.ErrInvalidRollback,
		},
		{
			name:    "explicit digest never deployed",
			request: func(history, []core.Deployment) rollback.Request { return rollback.Request{ToDigest: digestD} },
			wantErr: core.ErrInvalidRollback,
		},
		{
			name: "explicit target is blocked",
			setup: func(f *fakeRepo, _ history) []core.Deployment {
				f.addBlock(core.Block{Target: blog, Digest: digestA, CreatedBy: "user:bob"})
				return nil
			},
			request: func(h history, _ []core.Deployment) rollback.Request { return rollback.Request{ToDeployment: &h.v1.ID} },
			wantErr: core.ErrBlocked,
		},
		{
			name: "no earlier success",
			setup: func(f *fakeRepo, h history) []core.Deployment {
				f.addBlock(core.Block{Target: blog, Digest: digestA, CreatedBy: "user:bob"})
				f.addBlock(core.Block{Target: blog, Digest: digestB, CreatedBy: "user:bob"})
				return nil
			},
			wantErr: core.ErrNoRollbackTarget,
		},
		{
			name: "rollback already queued",
			setup: func(f *fakeRepo, h history) []core.Deployment {
				rb := deployment(blog, core.StateQueued, sha2, digestB)
				rb.Kind, rb.RollbackOfID = core.KindRollback, &h.v3.ID
				return []core.Deployment{f.add(rb)}
			},
			wantErr: core.ErrRollbackInProgress,
		},
		{
			name: "finished rollback does not block another",
			setup: func(f *fakeRepo, h history) []core.Deployment {
				rb := deployment(blog, core.StateSucceeded, sha2, digestB)
				rb.Kind, rb.RollbackOfID = core.KindRollback, &h.v3.ID
				f.addBlock(core.Block{Target: blog, Digest: digestC, CreatedBy: "user:bob"})
				return []core.Deployment{f.add(rb)}
			},
			want: want{rolledBack: extra(0), to: v1, blockDigest: digestB, blockCommit: sha2, rolledBackEnd: core.StateSucceeded},
		},
		{
			name: "rollback to a version deployed by an earlier rollback",
			setup: func(f *fakeRepo, h history) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateSucceeded, sha2, digestB))}
			},
			want: want{rolledBack: extra(0), to: v3, blockDigest: digestB, blockCommit: sha2, rolledBackEnd: core.StateSucceeded},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			h := seed(f)
			var extras []core.Deployment
			if tt.setup != nil {
				extras = tt.setup(f, h)
			}
			before := f.snapshot()
			svc, obs := newService(t, f)
			req := rollback.Request{}
			if tt.request != nil {
				req = tt.request(h, extras)
			}
			req.Target, req.Actor, req.Role = blog, alice, core.RoleDeployer

			got, err := svc.Rollback(t.Context(), req)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Rollback error = %v, want %v", err, tt.wantErr)
				}
				after := f.snapshot()
				if len(after.deployments) != len(before.deployments) || len(after.blocks) != len(before.blocks) || len(after.audit) != 0 {
					t.Errorf("failed rollback changed state: %d deployments, %d blocks, %d audit rows", len(after.deployments), len(after.blocks), len(after.audit))
				}
				if len(obs.calls()) != 0 {
					t.Errorf("observer called for a failed rollback")
				}
				return
			}
			if err != nil {
				t.Fatalf("Rollback: %v", err)
			}

			wantBad, wantTo := tt.want.rolledBack(h, extras), tt.want.to(h, extras)
			if got.RolledBack.ID != wantBad || got.RolledBack.State != tt.want.rolledBackEnd {
				t.Errorf("RolledBack = %d (%s), want %d (%s)", got.RolledBack.ID, got.RolledBack.State, wantBad, tt.want.rolledBackEnd)
			}
			if got.To.ID != wantTo {
				t.Errorf("To = %d, want %d", got.To.ID, wantTo)
			}
			rb := got.Rollback
			to := f.deployment(wantTo)
			if rb.Kind != core.KindRollback || rb.State != core.StateQueued || rb.Source != core.SourceAPI ||
				rb.RequestedBy != "user:alice" || rb.Commit != to.Commit || rb.Image != to.Image || rb.Digest != to.Digest ||
				rb.PreviousDigest != "" || rb.RollbackOfID == nil || *rb.RollbackOfID != wantBad || rb.Target != blog {
				t.Errorf("rollback deployment = %+v", rb)
			}
			after := f.snapshot()
			if p := after.priorities[rb.ID]; p != core.PriorityRollback {
				t.Errorf("rollback priority = %s, want rollback", p)
			}

			switch {
			case tt.want.noBlock:
				if got.Block != nil {
					t.Errorf("Block = %+v, want none", got.Block)
				}
			case got.Block == nil:
				t.Errorf("Block = nil, want one")
			default:
				b := got.Block
				if b.Digest != tt.want.blockDigest || b.Commit.SHA != tt.want.blockCommit || b.CreatedBy != "user:alice" ||
					b.Reason != "rolled back by user:alice" || b.DeploymentID == nil || *b.DeploymentID != wantBad ||
					b.RollbackID == nil || *b.RollbackID != rb.ID || !b.Active() {
					t.Errorf("Block = %+v, want digest %q commit %q", b, tt.want.blockDigest, tt.want.blockCommit)
				}
			}

			var wantCancelled []core.DeploymentID
			if tt.want.cancelled != nil {
				wantCancelled = tt.want.cancelled(h, extras)
			}
			if !slices.Equal(got.Cancelled, wantCancelled) {
				t.Errorf("Cancelled = %v, want %v", got.Cancelled, wantCancelled)
			}
			for _, id := range wantCancelled {
				if s := f.deployment(id).State; s != core.StateCancelled {
					t.Errorf("deployment %d is %s, want cancelled", id, s)
				}
			}
			if tt.want.stillQueued != nil {
				for _, id := range tt.want.stillQueued(h, extras) {
					if s := f.deployment(id).State; s != core.StateQueued {
						t.Errorf("deployment %d is %s, want it still queued", id, s)
					}
				}
			}

			if len(after.audit) != 1 || after.audit[0].Action != core.AuditRollbackRequest ||
				after.audit[0].DeploymentID == nil || *after.audit[0].DeploymentID != rb.ID {
				t.Errorf("audit = %+v, want one rollback.request for %d", after.audit, rb.ID)
			}
			if f.locked[0] != blog {
				t.Errorf("locked %v, want %s", f.locked, blog)
			}
			if triggers := obs.triggered(); !slices.Equal(triggers, []rollback.Trigger{rollback.TriggerManual}) {
				t.Errorf("observer triggers = %v, want manual", triggers)
			}
			if calls := obs.calls(); !slices.Equal(calls, []string{"blog"}) {
				t.Errorf("observer calls = %v", calls)
			}
		})
	}
}

func TestRollbackAuditDetail(t *testing.T) {
	t.Parallel()

	f := newFakeRepo()
	h := seed(f)
	inflight := f.add(deployment(blog, core.StatePinning, sha4, digestD))
	queued := f.add(deployment(blog, core.StateQueued, sha4, ""))
	svc, _ := newService(t, f)

	got, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, Role: core.RoleAdmin, Reason: "  error rate doubled  "})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got.Block.Reason != "error rate doubled" {
		t.Errorf("block reason = %q, want the trimmed request reason", got.Block.Reason)
	}
	audit := f.snapshot().audit
	if len(audit) != 1 {
		t.Fatalf("audit = %+v", audit)
	}
	entry := audit[0]
	if entry.Actor != alice || entry.Role != core.RoleAdmin || entry.Target == nil || *entry.Target != blog {
		t.Errorf("audit entry = %+v", entry)
	}
	var detail map[string]any
	if err := json.Unmarshal(entry.Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	want := map[string]any{
		"rolled_back_deployment_id": float64(inflight.ID),
		"to_deployment_id":          float64(h.v3.ID),
		"digest":                    string(digestC),
		"blocked_digest":            string(digestD),
		"blocked_commit":            sha4,
		"block_id":                  float64(got.Block.ID),
		"reason":                    "error rate doubled",
		"cancelled":                 []any{float64(inflight.ID), float64(queued.ID)},
	}
	if !mapsEqual(detail, want) {
		t.Errorf("detail = %v, want %v", detail, want)
	}
}

func mapsEqual(a, b map[string]any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb) && slices.Equal(slices.Sorted(maps.Keys(a)), slices.Sorted(maps.Keys(b)))
}

func TestRollbackRetriesStateConflicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		conflicts int
		wantErr   error
	}{
		{name: "succeeds after two conflicts", conflicts: rollback.MaxAttempts - 1},
		{name: "gives up after max attempts", conflicts: rollback.MaxAttempts, wantErr: core.ErrStateConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			seed(f)
			inflight := f.add(deployment(blog, core.StateSyncing, sha4, digestD))
			f.conflicts = tt.conflicts
			svc, _ := newService(t, f)

			got, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice})
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("Rollback error = %v, want %v", err, tt.wantErr)
			}
			state := f.snapshot()
			if f.txs != min(tt.conflicts+1, rollback.MaxAttempts) {
				t.Errorf("ran %d transactions", f.txs)
			}
			if tt.wantErr != nil {
				if len(state.deployments) != 4 || len(state.blocks) != 0 || len(state.audit) != 0 || f.deployment(inflight.ID).State != core.StateSyncing {
					t.Errorf("state after giving up: %d deployments, %d blocks, %d audit rows", len(state.deployments), len(state.blocks), len(state.audit))
				}
				return
			}
			if got.RolledBack.ID != inflight.ID || got.RolledBack.State != core.StateCancelled {
				t.Errorf("RolledBack = %+v", got.RolledBack)
			}
		})
	}
}

func TestRollbackInProgressReturnsTheRollback(t *testing.T) {
	t.Parallel()

	f := newFakeRepo()
	seed(f)
	svc, _ := newService(t, f)
	first, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice})
	if err != nil {
		t.Fatalf("first Rollback: %v", err)
	}
	_, err = svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice})
	var inProgress *core.RollbackInProgressError
	if !errors.As(err, &inProgress) || inProgress.Deployment.ID != first.Rollback.ID {
		t.Fatalf("second Rollback error = %v, want rollback %d in progress", err, first.Rollback.ID)
	}
}

func TestRollbackValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  rollback.Request
	}{
		{name: "both targets", req: rollback.Request{Target: blog, Actor: alice, ToDeployment: idPtr(1), ToDigest: digestA}},
		{name: "bad digest", req: rollback.Request{Target: blog, Actor: alice, ToDigest: "sha256:x"}},
		{name: "zero deployment", req: rollback.Request{Target: blog, Actor: alice, ToDeployment: idPtr(0)}},
		{name: "reason with newline", req: rollback.Request{Target: blog, Actor: alice, Reason: "a\nb"}},
		{name: "reason too long", req: rollback.Request{Target: blog, Actor: alice, Reason: strings.Repeat("x", core.MaxBlockReasonLength+1)}},
		{name: "no actor", req: rollback.Request{Target: blog}},
		{name: "no target", req: rollback.Request{Actor: alice}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			seed(f)
			svc, _ := newService(t, f)
			if _, err := svc.Rollback(t.Context(), tt.req); !errors.Is(err, core.ErrInvalidRollback) {
				t.Fatalf("Rollback error = %v, want ErrInvalidRollback", err)
			}
			if f.txs != 0 {
				t.Errorf("an invalid request opened %d transactions", f.txs)
			}
		})
	}
}

func TestRollbackRepositoryErrors(t *testing.T) {
	t.Parallel()

	for _, op := range []string{"LockTarget", "ListDeployments", "CurrentDeployment", "MigrationsBetween", "EnqueueDeployment", "CreateBlock", "RecordAudit"} {
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			seed(f)
			f.errs[op] = errBoom
			svc, obs := newService(t, f)
			if _, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice}); !errors.Is(err, errBoom) {
				t.Fatalf("Rollback error = %v, want %v", err, errBoom)
			}
			if state := f.snapshot(); len(state.deployments) != 3 || len(state.blocks) != 0 {
				t.Errorf("state after %s failed: %d deployments, %d blocks", op, len(state.deployments), len(state.blocks))
			}
			if len(obs.calls()) != 0 {
				t.Error("observer called")
			}
		})
	}
}

func TestBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		existing    *core.Block
		req         rollback.BlockRequest
		wantCreated bool
		wantErr     error
	}{
		{
			name:        "digest",
			req:         rollback.BlockRequest{Ref: core.BlockRef{Digest: digestA}, Reason: " leaks memory "},
			wantCreated: true,
		},
		{
			name:        "commit",
			req:         rollback.BlockRequest{Ref: core.BlockRef{CommitSHA: sha1}, Reason: "bad migration"},
			wantCreated: true,
		},
		{
			name:     "already blocked",
			existing: &core.Block{Target: blog, Digest: digestA, Reason: "first", CreatedBy: "user:bob"},
			req:      rollback.BlockRequest{Ref: core.BlockRef{Digest: digestA}, Reason: "again"},
		},
		{name: "no reason", req: rollback.BlockRequest{Ref: core.BlockRef{Digest: digestA}, Reason: "  "}, wantErr: core.ErrInvalidBlock},
		{name: "reason with tab", req: rollback.BlockRequest{Ref: core.BlockRef{Digest: digestA}, Reason: "a\tb"}, wantErr: core.ErrInvalidBlock},
		{name: "no ref", req: rollback.BlockRequest{Reason: "x"}, wantErr: core.ErrInvalidBlock},
		{name: "no actor", req: rollback.BlockRequest{Ref: core.BlockRef{Digest: digestA}, Reason: "x", Actor: core.Actor{Kind: core.ActorUser}}, wantErr: core.ErrInvalidBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			if tt.existing != nil {
				f.addBlock(*tt.existing)
			}
			svc, _ := newService(t, f)
			req := tt.req
			req.Target, req.Role = blog, core.RoleDeployer
			if req.Actor == (core.Actor{}) {
				req.Actor = alice
			}
			got, created, err := svc.Block(t.Context(), req)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("Block error = %v, want %v", err, tt.wantErr)
			}
			audit := f.snapshot().audit
			if err != nil {
				if len(audit) != 0 {
					t.Errorf("audit = %+v, want none", audit)
				}
				return
			}
			if created != tt.wantCreated || got.ID == 0 || got.Ref().String() != req.Ref.String() {
				t.Errorf("Block = %+v, created %v, want created %v", got, created, tt.wantCreated)
			}
			if !tt.wantCreated {
				if len(audit) != 0 || got.Reason != "first" {
					t.Errorf("existing block: %+v, audit %+v", got, audit)
				}
				return
			}
			if got.CreatedBy != "user:alice" || got.Reason != strings.TrimSpace(req.Reason) {
				t.Errorf("Block = %+v", got)
			}
			if len(audit) != 1 || audit[0].Action != core.AuditBlockCreate || audit[0].Role != core.RoleDeployer || *audit[0].Target != blog {
				t.Errorf("audit = %+v, want one block.create", audit)
			}
		})
	}
}

func TestUnblock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ref       core.BlockRef
		wantCount int
		wantErr   error
	}{
		{name: "digest", ref: core.BlockRef{Digest: digestA}, wantCount: 1},
		{name: "commit clears every block of it", ref: core.BlockRef{CommitSHA: sha1}, wantCount: 2},
		{name: "not blocked", ref: core.BlockRef{Digest: digestC}, wantErr: core.ErrNotFound},
		{name: "invalid ref", ref: core.BlockRef{CommitSHA: "nope"}, wantErr: core.ErrInvalidBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			f.addBlock(core.Block{Target: blog, Digest: digestA, Commit: core.Commit{SHA: sha1}, CreatedBy: "user:bob"})
			f.addBlock(core.Block{Target: blog, Digest: digestB, Commit: core.Commit{SHA: sha1}, CreatedBy: "user:bob"})
			svc, _ := newService(t, f)

			got, err := svc.Unblock(t.Context(), blog, tt.ref, alice, core.RoleDeployer)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
				t.Fatalf("Unblock error = %v, want %v", err, tt.wantErr)
			}
			audit := f.snapshot().audit
			if err != nil {
				if len(audit) != 0 {
					t.Errorf("audit = %+v, want none", audit)
				}
				return
			}
			if len(got) != tt.wantCount {
				t.Fatalf("Unblock cleared %d blocks, want %d", len(got), tt.wantCount)
			}
			for _, b := range got {
				if b.Active() || b.ClearedBy != "user:alice" {
					t.Errorf("cleared block = %+v", b)
				}
			}
			if len(audit) != 1 || audit[0].Action != core.AuditBlockClear {
				t.Errorf("audit = %+v, want one block.clear", audit)
			}
			active, err := svc.ListBlocks(t.Context(), blog)
			if err != nil || len(active) != 2-tt.wantCount {
				t.Errorf("ListBlocks = %d, %v, want %d", len(active), err, 2-tt.wantCount)
			}
		})
	}
}

func TestEnginePorts(t *testing.T) {
	t.Parallel()

	f := newFakeRepo()
	block := f.addBlock(core.Block{Target: blog, Digest: digestA, Commit: core.Commit{SHA: sha1}, CreatedBy: "user:bob"})
	commitOnly := f.addBlock(core.Block{Target: blog, Commit: core.Commit{SHA: sha2}, CreatedBy: "user:bob"})
	f.trailers = core.BlockTrailers{Blocks: []core.Digest{digestA}, Unblocks: []core.Digest{digestB}}
	svc, _ := newService(t, f)

	tests := []struct {
		name   string
		commit string
		digest core.Digest
		want   core.BlockID
	}{
		{name: "by digest", commit: sha3, digest: digestA, want: block.ID},
		{name: "by commit", commit: sha1, digest: digestC, want: block.ID},
		{name: "commit-only block", commit: sha2, digest: digestC, want: commitOnly.ID},
		{name: "not blocked", commit: sha3, digest: digestC},
		{name: "digest not yet resolved", commit: sha2, want: commitOnly.ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := svc.Blocked(t.Context(), blog, core.Commit{Repository: "acme/blog", SHA: tt.commit}, tt.digest)
			if err != nil || found != (tt.want != 0) || got.ID != tt.want {
				t.Fatalf("Blocked = %d, %v, %v, want %d", got.ID, found, err, tt.want)
			}
		})
	}

	trailers, err := svc.PendingTrailers(t.Context(), blog)
	if err != nil || !slices.Equal(trailers.Blocks, f.trailers.Blocks) || !slices.Equal(trailers.Unblocks, f.trailers.Unblocks) {
		t.Fatalf("PendingTrailers = %+v, %v", trailers, err)
	}
	pin := core.Commit{Repository: "acme/gitops", SHA: sha5}
	if err := svc.MarkMirrored(t.Context(), blog, trailers, pin); err != nil {
		t.Fatalf("MarkMirrored: %v", err)
	}
	if m := f.snapshot().mirrored; len(m) != 1 || m[0].commit != pin || m[0].target != blog {
		t.Fatalf("mirrored = %+v", m)
	}
}
