package rollback_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
)

var doorstep = core.Actor{Kind: core.ActorSystem, ID: "doorstep"}

const autoReason = "automatic rollback of deployment 4: after gates failed: e2e"

type failRecorder struct {
	mu    sync.Mutex
	plans []rollback.Plan
}

func (r *failRecorder) fail(bad core.Deployment) func(rollback.Plan) (store.Transition, error) {
	return func(p rollback.Plan) (store.Transition, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.plans = append(r.plans, p)
		data, err := json.Marshal(map[string]any{"outcome": p.Outcome})
		return store.Transition{
			ID: bad.ID, From: bad.State, To: core.StateFailed, Actor: "doorstep",
			Message: "failed while verifying", Data: data, FailureReason: "after gates failed: e2e",
		}, err
	}
}

func (r *failRecorder) seen() []rollback.Plan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.plans)
}

func automaticRequest(bad core.Deployment, fail func(rollback.Plan) (store.Transition, error)) rollback.AutomaticRequest {
	return rollback.AutomaticRequest{Deployment: bad, Trigger: rollback.TriggerGateFailed, Reason: autoReason, Actor: doorstep, Fail: fail}
}

func failing(sha string, digest core.Digest) core.Deployment {
	d := deployment(blog, core.StateVerifying, sha, digest)
	d.PinCommitSHA = shaOf('f')
	return d
}

func TestAutomaticQueuesARollback(t *testing.T) {
	t.Parallel()

	type want struct {
		to          func(h history) core.DeploymentID
		blockCommit string
		cancelled   func(extra []core.Deployment) []core.DeploymentID
		stillQueued func(extra []core.Deployment) []core.DeploymentID
	}
	v2 := func(h history) core.DeploymentID { return h.v2.ID }
	v3 := func(h history) core.DeploymentID { return h.v3.ID }
	extra := func(indexes ...int) func([]core.Deployment) []core.DeploymentID {
		return func(e []core.Deployment) []core.DeploymentID {
			var ids []core.DeploymentID
			for _, i := range indexes {
				ids = append(ids, e[i].ID)
			}
			return ids
		}
	}

	tests := []struct {
		name  string
		bad   core.Deployment
		setup func(f *fakeRepo) []core.Deployment
		want  want
	}{
		{
			name: "previous success",
			bad:  failing(sha4, digestD),
			want: want{to: v3, blockCommit: sha4},
		},
		{
			name: "blocked digests are skipped",
			bad:  failing(sha4, digestD),
			setup: func(f *fakeRepo) []core.Deployment {
				f.addBlock(core.Block{Target: blog, Digest: digestC, CreatedBy: "user:bob"})
				return nil
			},
			want: want{to: v2, blockCommit: sha4},
		},
		{
			name: "queued deploys of the blocked commit are cancelled, newer ones stay",
			bad:  failing(sha4, digestD),
			setup: func(f *fakeRepo) []core.Deployment {
				return []core.Deployment{
					f.add(deployment(blog, core.StateQueued, sha4, "")),
					f.add(deployment(blog, core.StateQueued, sha5, "")),
					f.add(deployment(web, core.StateQueued, sha4, "")),
				}
			},
			want: want{to: v3, blockCommit: sha4, cancelled: extra(0), stillQueued: extra(1, 2)},
		},
		{
			name: "a rebuild of the target commit blocks only the digest",
			bad:  failing(sha3, digestD),
			setup: func(f *fakeRepo) []core.Deployment {
				return []core.Deployment{f.add(deployment(blog, core.StateQueued, sha3, ""))}
			},
			want: want{to: v3, stillQueued: extra(0)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			h := seed(f)
			var extras []core.Deployment
			if tt.setup != nil {
				extras = tt.setup(f)
			}
			bad := f.add(tt.bad)
			svc, obs := newService(t, f)
			rec := &failRecorder{}

			got, err := svc.Automatic(t.Context(), automaticRequest(bad, rec.fail(bad)))
			if err != nil {
				t.Fatalf("Automatic: %v", err)
			}
			to := f.deployment(tt.want.to(h))
			plan := got.Plan
			if plan.Outcome != rollback.OutcomeQueued || plan.To == nil || plan.To.ID != to.ID || plan.Rollback == nil || len(plan.Migrations) != 0 {
				t.Fatalf("plan = %+v, want queued to %d", plan, to.ID)
			}
			if seen := rec.seen(); len(seen) != 1 || seen[0].Rollback == nil || seen[0].Rollback.ID != plan.Rollback.ID {
				t.Errorf("Fail saw %+v, want the plan with the queued rollback", seen)
			}

			rb := f.deployment(plan.Rollback.ID)
			if rb.Kind != core.KindRollback || rb.State != core.StateQueued || rb.Source != core.SourceSystem ||
				rb.RequestedBy != "system:doorstep" || rb.Commit != to.Commit || rb.Image != to.Image || rb.Digest != to.Digest ||
				rb.RollbackOfID == nil || *rb.RollbackOfID != bad.ID || rb.Migration {
				t.Errorf("rollback deployment = %+v", rb)
			}
			after := f.snapshot()
			if p := after.priorities[rb.ID]; p != core.PriorityRollback {
				t.Errorf("rollback priority = %s, want rollback", p)
			}
			if got.Failed.ID != bad.ID || got.Failed.State != core.StateFailed || f.deployment(bad.ID).FailureReason != "after gates failed: e2e" {
				t.Errorf("failed = %+v, want deployment %d failed", got.Failed, bad.ID)
			}

			b := got.Block
			if b == nil || b.Digest != bad.Digest || b.Commit.SHA != tt.want.blockCommit || b.CreatedBy != "system:doorstep" ||
				b.Reason != autoReason || b.DeploymentID == nil || *b.DeploymentID != bad.ID || b.RollbackID == nil || *b.RollbackID != rb.ID {
				t.Errorf("block = %+v, want digest %s commit %q", b, bad.Digest, tt.want.blockCommit)
			}

			var wantCancelled []core.DeploymentID
			if tt.want.cancelled != nil {
				wantCancelled = tt.want.cancelled(extras)
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
				for _, id := range tt.want.stillQueued(extras) {
					if s := f.deployment(id).State; s != core.StateQueued {
						t.Errorf("deployment %d is %s, want it still queued", id, s)
					}
				}
			}

			if len(after.audit) != 1 {
				t.Fatalf("audit = %+v, want one entry", after.audit)
			}
			entry := after.audit[0]
			if entry.Action != core.AuditRollbackRequest || entry.Actor != doorstep || entry.Role != "" ||
				entry.DeploymentID == nil || *entry.DeploymentID != rb.ID || entry.Target == nil || *entry.Target != blog {
				t.Errorf("audit entry = %+v", entry)
			}
			if f.locked[0] != blog {
				t.Errorf("locked %v, want %s", f.locked, blog)
			}
			if triggers := obs.triggered(); !slices.Equal(triggers, []rollback.Trigger{rollback.TriggerGateFailed}) {
				t.Errorf("observer triggers = %v, want gate_failed", triggers)
			}
		})
	}
}

func TestAutomaticAuditDetail(t *testing.T) {
	t.Parallel()

	f := newFakeRepo()
	h := seed(f)
	queued := f.add(deployment(blog, core.StateQueued, sha4, ""))
	bad := f.add(failing(sha4, digestD))
	svc, _ := newService(t, f)
	req := automaticRequest(bad, (&failRecorder{}).fail(bad))
	req.Trigger = rollback.TriggerSyncFailed

	got, err := svc.Automatic(t.Context(), req)
	if err != nil {
		t.Fatalf("Automatic: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(f.snapshot().audit[0].Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	want := map[string]any{
		"rolled_back_deployment_id": float64(bad.ID),
		"to_deployment_id":          float64(h.v3.ID),
		"digest":                    string(digestC),
		"blocked_digest":            string(digestD),
		"blocked_commit":            sha4,
		"block_id":                  float64(got.Block.ID),
		"reason":                    autoReason,
		"cancelled":                 []any{float64(queued.ID)},
		"automatic":                 true,
		"trigger":                   "sync_failed",
	}
	if !mapsEqual(detail, want) {
		t.Errorf("detail = %v, want %v", detail, want)
	}
}

func TestAutomaticWithholds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		setup          func(f *fakeRepo, h *history) core.Deployment
		wantOutcome    rollback.Outcome
		wantTo         func(h history) core.DeploymentID
		wantMigrations func(h history, bad core.Deployment) []core.DeploymentID
	}{
		{
			name: "nothing succeeded before",
			setup: func(f *fakeRepo, _ *history) core.Deployment {
				return f.add(failing(sha1, digestA))
			},
			wantOutcome: rollback.OutcomeNoTarget,
		},
		{
			name: "every earlier success is blocked",
			setup: func(f *fakeRepo, h *history) core.Deployment {
				*h = seed(f)
				for _, d := range []core.Digest{digestA, digestB, digestC} {
					f.addBlock(core.Block{Target: blog, Digest: d, CreatedBy: "user:bob"})
				}
				return f.add(failing(sha4, digestD))
			},
			wantOutcome: rollback.OutcomeNoTarget,
		},
		{
			name: "the failing digest is the last success",
			setup: func(f *fakeRepo, h *history) core.Deployment {
				*h = seed(f)
				return f.add(failing(sha4, digestC))
			},
			wantOutcome: rollback.OutcomeSameVersion,
			wantTo:      func(h history) core.DeploymentID { return h.v3.ID },
		},
		{
			name: "the failing release has migrations",
			setup: func(f *fakeRepo, h *history) core.Deployment {
				*h = seed(f)
				bad := failing(sha4, digestD)
				bad.Migration, bad.MigrationReason = true, "trailer Doorstep-Migration in 4444444"
				return f.add(bad)
			},
			wantOutcome:    rollback.OutcomeWithheldMigrations,
			wantTo:         func(h history) core.DeploymentID { return h.v3.ID },
			wantMigrations: func(_ history, bad core.Deployment) []core.DeploymentID { return []core.DeploymentID{bad.ID} },
		},
		{
			name: "a rollback is already queued",
			setup: func(f *fakeRepo, h *history) core.Deployment {
				*h = seed(f)
				rb := deployment(blog, core.StateQueued, sha2, digestB)
				rb.Kind = core.KindRollback
				f.add(rb)
				return f.add(failing(sha4, digestD))
			},
			wantOutcome: rollback.OutcomeRollbackInProgress,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			var h history
			bad := tt.setup(f, &h)
			before := f.snapshot()
			svc, obs := newService(t, f)
			rec := &failRecorder{}

			got, err := svc.Automatic(t.Context(), automaticRequest(bad, rec.fail(bad)))
			if err != nil {
				t.Fatalf("Automatic: %v", err)
			}
			if got.Plan.Outcome != tt.wantOutcome || got.Plan.Rollback != nil || got.Block != nil || len(got.Cancelled) != 0 {
				t.Fatalf("result = %+v, want outcome %s and nothing queued", got, tt.wantOutcome)
			}
			if tt.wantTo == nil && got.Plan.To != nil || tt.wantTo != nil && (got.Plan.To == nil || got.Plan.To.ID != tt.wantTo(h)) {
				t.Errorf("plan.To = %+v", got.Plan.To)
			}
			var wantMigrations []core.DeploymentID
			if tt.wantMigrations != nil {
				wantMigrations = tt.wantMigrations(h, bad)
			}
			var migrations []core.DeploymentID
			for _, d := range got.Plan.Migrations {
				migrations = append(migrations, d.ID)
			}
			if !slices.Equal(migrations, wantMigrations) {
				t.Errorf("plan.Migrations = %v, want %v", migrations, wantMigrations)
			}
			if seen := rec.seen(); len(seen) != 1 || seen[0].Outcome != tt.wantOutcome {
				t.Errorf("Fail saw %+v", seen)
			}

			after := f.snapshot()
			if got.Failed.State != core.StateFailed || f.deployment(bad.ID).State != core.StateFailed {
				t.Errorf("failing deployment = %s, want failed", f.deployment(bad.ID).State)
			}
			if len(after.deployments) != len(before.deployments) || len(after.blocks) != len(before.blocks) || len(after.audit) != 0 {
				t.Errorf("writes = %d deployments, %d blocks, %d audit rows; want only the failure", len(after.deployments)-len(before.deployments), len(after.blocks)-len(before.blocks), len(after.audit))
			}
			if len(after.transitions) != 1 || after.transitions[0].ID != bad.ID {
				t.Errorf("transitions = %+v, want only the failure", after.transitions)
			}
			if calls := obs.calls(); len(calls) != 0 {
				t.Errorf("observer calls = %v, want none", calls)
			}
		})
	}
}

func TestAutomaticAbortsEverything(t *testing.T) {
	t.Parallel()

	errEncode := errors.New("encode failure")
	tests := []struct {
		name    string
		fail    func(bad core.Deployment) func(rollback.Plan) (store.Transition, error)
		errs    map[string]error
		wantErr error
	}{
		{
			name: "the failure transition cannot be built",
			fail: func(core.Deployment) func(rollback.Plan) (store.Transition, error) {
				return func(rollback.Plan) (store.Transition, error) { return store.Transition{}, errEncode }
			},
			wantErr: errEncode,
		},
		{
			name: "the transition does not fail the deployment",
			fail: func(bad core.Deployment) func(rollback.Plan) (store.Transition, error) {
				return func(rollback.Plan) (store.Transition, error) {
					return store.Transition{ID: bad.ID, From: bad.State, To: core.StateCancelled}, nil
				}
			},
		},
		{
			name: "the transition names another deployment",
			fail: func(bad core.Deployment) func(rollback.Plan) (store.Transition, error) {
				return func(rollback.Plan) (store.Transition, error) {
					return store.Transition{ID: bad.ID + 1, From: bad.State, To: core.StateFailed, FailureReason: "x"}, nil
				}
			},
		},
		{
			name: "the deployment moved on",
			fail: func(bad core.Deployment) func(rollback.Plan) (store.Transition, error) {
				return func(rollback.Plan) (store.Transition, error) {
					return store.Transition{ID: bad.ID, From: core.StateSyncing, To: core.StateFailed, FailureReason: "x"}, nil
				}
			},
			wantErr: core.ErrStateConflict,
		},
		{
			name:    "the block cannot be written",
			fail:    (&failRecorder{}).fail,
			errs:    map[string]error{"CreateBlock": errBoom},
			wantErr: errBoom,
		},
		{
			name:    "the audit row cannot be written",
			fail:    (&failRecorder{}).fail,
			errs:    map[string]error{"RecordAudit": errBoom},
			wantErr: errBoom,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			seed(f)
			f.add(deployment(blog, core.StateQueued, sha4, ""))
			bad := f.add(failing(sha4, digestD))
			before := f.snapshot()
			for op, err := range tt.errs {
				f.errs[op] = err
			}
			svc, obs := newService(t, f)

			_, err := svc.Automatic(t.Context(), automaticRequest(bad, tt.fail(bad)))
			if err == nil {
				t.Fatal("Automatic succeeded")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			after := f.snapshot()
			if !slices.EqualFunc(after.deployments, before.deployments, func(a, b core.Deployment) bool { return a.ID == b.ID && a.State == b.State }) ||
				len(after.blocks) != 0 || len(after.audit) != 0 || len(after.transitions) != 0 {
				t.Errorf("state changed: %d deployments, %d blocks, %d audit rows, %d transitions", len(after.deployments), len(after.blocks), len(after.audit), len(after.transitions))
			}
			if calls := obs.calls(); len(calls) != 0 {
				t.Errorf("observer calls = %v, want none", calls)
			}
		})
	}
}

func TestAutomaticRetriesStateConflicts(t *testing.T) {
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
			bad := f.add(failing(sha4, digestD))
			f.conflicts = tt.conflicts
			svc, _ := newService(t, f)

			got, err := svc.Automatic(t.Context(), automaticRequest(bad, (&failRecorder{}).fail(bad)))
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("Automatic = %v, want %v", err, tt.wantErr)
			}
			if f.txs != min(tt.conflicts+1, rollback.MaxAttempts) {
				t.Errorf("transactions = %d", f.txs)
			}
			rollbacks := 0
			for _, d := range f.snapshot().deployments {
				if d.Kind == core.KindRollback {
					rollbacks++
				}
			}
			wantRollbacks := 1
			if tt.wantErr != nil {
				wantRollbacks = 0
			}
			if rollbacks != wantRollbacks || (tt.wantErr == nil && got.Plan.Outcome != rollback.OutcomeQueued) {
				t.Errorf("rollback deployments = %d, want %d", rollbacks, wantRollbacks)
			}
		})
	}
}

func TestAutomaticValidatesTheRequest(t *testing.T) {
	t.Parallel()

	bad := failing(sha4, digestD)
	bad.ID = 4
	tests := []struct {
		name   string
		mutate func(r *rollback.AutomaticRequest)
	}{
		{name: "rollback", mutate: func(r *rollback.AutomaticRequest) { r.Deployment.Kind = core.KindRollback }},
		{name: "terminal deployment", mutate: func(r *rollback.AutomaticRequest) { r.Deployment.State = core.StateFailed }},
		{name: "no deployment id", mutate: func(r *rollback.AutomaticRequest) { r.Deployment.ID = 0 }},
		{name: "manual trigger", mutate: func(r *rollback.AutomaticRequest) { r.Trigger = rollback.TriggerManual }},
		{name: "unknown trigger", mutate: func(r *rollback.AutomaticRequest) { r.Trigger = "cosmic_ray" }},
		{name: "no failure transition", mutate: func(r *rollback.AutomaticRequest) { r.Fail = nil }},
		{name: "reason too long", mutate: func(r *rollback.AutomaticRequest) { r.Reason = strings.Repeat("x", core.MaxBlockReasonLength+1) }},
		{name: "invalid actor", mutate: func(r *rollback.AutomaticRequest) { r.Actor = core.Actor{} }},
		{name: "invalid target", mutate: func(r *rollback.AutomaticRequest) { r.Deployment.Target = core.Target{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			svc, _ := newService(t, f)
			req := automaticRequest(bad, (&failRecorder{}).fail(bad))
			tt.mutate(&req)

			if _, err := svc.Automatic(t.Context(), req); !errors.Is(err, core.ErrInvalidRollback) {
				t.Errorf("Automatic = %v, want ErrInvalidRollback", err)
			}
			if f.txs != 0 {
				t.Errorf("transactions = %d, want none", f.txs)
			}
		})
	}
}

func TestAutomaticMigrationGuard(t *testing.T) {
	t.Parallel()

	flagged := func(d core.Deployment) core.Deployment {
		d.Migration, d.MigrationReason = true, "label db-migration on #12"
		return d
	}
	tests := []struct {
		name           string
		setup          func(f *fakeRepo) core.Deployment
		wantOutcome    rollback.Outcome
		wantMigrations int
	}{
		{
			name: "the failing deploy is flagged",
			setup: func(f *fakeRepo) core.Deployment {
				f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				return f.add(flagged(failing(sha2, digestB)))
			},
			wantOutcome:    rollback.OutcomeWithheldMigrations,
			wantMigrations: 1,
		},
		{
			name: "an intermediate release that never ran is flagged",
			setup: func(f *fakeRepo) core.Deployment {
				f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				f.add(flagged(deployment(blog, core.StateSuperseded, sha2, "")))
				return f.add(failing(sha3, digestC))
			},
			wantOutcome:    rollback.OutcomeWithheldMigrations,
			wantMigrations: 1,
		},
		{
			name: "the target itself is flagged",
			setup: func(f *fakeRepo) core.Deployment {
				f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				f.add(flagged(deployment(blog, core.StateSucceeded, sha2, digestB)))
				return f.add(failing(sha3, digestC))
			},
			wantOutcome: rollback.OutcomeQueued,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			bad := tt.setup(f)
			svc, _ := newService(t, f)

			got, err := svc.Automatic(t.Context(), automaticRequest(bad, (&failRecorder{}).fail(bad)))
			if err != nil {
				t.Fatalf("Automatic: %v", err)
			}
			if got.Plan.Outcome != tt.wantOutcome || len(got.Plan.Migrations) != tt.wantMigrations {
				t.Errorf("plan = %s with %d migrations, want %s with %d", got.Plan.Outcome, len(got.Plan.Migrations), tt.wantOutcome, tt.wantMigrations)
			}
			if tt.wantOutcome == rollback.OutcomeQueued && (got.Plan.Rollback == nil || got.Plan.Rollback.Migration) {
				t.Errorf("rollback = %+v, want an unflagged rollback", got.Plan.Rollback)
			}
		})
	}
}
