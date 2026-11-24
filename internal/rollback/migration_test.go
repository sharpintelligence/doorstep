package rollback_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func flagged(d core.Deployment, reason string) core.Deployment {
	d.Migration = true
	d.MigrationReason = reason
	return d
}

func TestRollbackAcrossMigrations(t *testing.T) {
	t.Parallel()

	type ids struct {
		to         core.DeploymentID
		rolledBack core.DeploymentID
		crossed    []core.DeploymentID
	}
	tests := []struct {
		name    string
		setup   func(f *fakeRepo) ids
		request func(ids) rollback.Request
		confirm bool
		wantErr error
	}{
		{
			name: "crossing a flagged release needs confirmation",
			setup: func(f *fakeRepo) ids {
				v1 := f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				v2 := f.add(flagged(deployment(blog, core.StateSucceeded, sha2, digestB), "label db-migration on #12 (2222222)"))
				v3 := f.add(deployment(blog, core.StateSucceeded, sha3, digestC))
				return ids{to: v1.ID, rolledBack: v3.ID, crossed: []core.DeploymentID{v2.ID}}
			},
			request: func(i ids) rollback.Request { return rollback.Request{ToDeployment: &i.to} },
			wantErr: core.ErrMigrationConfirmationRequired,
		},
		{
			name: "confirmed crossing proceeds",
			setup: func(f *fakeRepo) ids {
				v1 := f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				v2 := f.add(flagged(deployment(blog, core.StateSucceeded, sha2, digestB), "trailer"))
				v3 := f.add(flagged(deployment(blog, core.StateSucceeded, sha3, digestC), "trailer"))
				return ids{to: v1.ID, rolledBack: v3.ID, crossed: []core.DeploymentID{v2.ID, v3.ID}}
			},
			request: func(i ids) rollback.Request { return rollback.Request{ToDeployment: &i.to} },
			confirm: true,
		},
		{
			name: "rolling back the flagged release itself needs confirmation",
			setup: func(f *fakeRepo) ids {
				v1 := f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				v2 := f.add(flagged(deployment(blog, core.StateSucceeded, sha2, digestB), "trailer"))
				return ids{to: v1.ID, rolledBack: v2.ID, crossed: []core.DeploymentID{v2.ID}}
			},
			wantErr: core.ErrMigrationConfirmationRequired,
		},
		{
			name: "rolling back to a flagged release needs nothing",
			setup: func(f *fakeRepo) ids {
				v1 := f.add(flagged(deployment(blog, core.StateSucceeded, sha1, digestA), "trailer"))
				v2 := f.add(deployment(blog, core.StateSucceeded, sha2, digestB))
				return ids{to: v1.ID, rolledBack: v2.ID}
			},
		},
		{
			name: "superseded flagged release counts",
			setup: func(f *fakeRepo) ids {
				v1 := f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				v2 := f.add(flagged(deployment(blog, core.StateSuperseded, sha2, ""), "trailer"))
				v3 := f.add(deployment(blog, core.StateSucceeded, sha3, digestC))
				return ids{to: v1.ID, rolledBack: v3.ID, crossed: []core.DeploymentID{v2.ID}}
			},
			wantErr: core.ErrMigrationConfirmationRequired,
		},
		{
			name: "confirmation is harmless without a migration",
			setup: func(f *fakeRepo) ids {
				h := seed(f)
				return ids{to: h.v2.ID, rolledBack: h.v3.ID}
			},
			confirm: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			want := tt.setup(f)
			svc, obs := newService(t, f)
			req := rollback.Request{}
			if tt.request != nil {
				req = tt.request(want)
			}
			req.Target, req.Actor, req.Role, req.ConfirmMigration = blog, alice, core.RoleDeployer, tt.confirm
			before := f.snapshot()

			got, err := svc.Rollback(t.Context(), req)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Rollback error = %v, want %v", err, tt.wantErr)
				}
				crossing, ok := errors.AsType[*core.MigrationRollbackError](err)
				if !ok || crossing.To.ID != want.to || !slices.Equal(crossing.IDs(), want.crossed) {
					t.Fatalf("MigrationRollbackError = %+v, want to %d across %v", crossing, want.to, want.crossed)
				}
				after := f.snapshot()
				if len(after.deployments) != len(before.deployments) || len(after.blocks) != 0 || len(after.audit) != 0 {
					t.Errorf("a refused rollback wrote %d deployments, %d blocks, %d audit rows", len(after.deployments)-len(before.deployments), len(after.blocks), len(after.audit))
				}
				if f.txs != 1 {
					t.Errorf("ran %d transactions, want 1 without retries", f.txs)
				}
				if len(obs.calls()) != 0 {
					t.Error("observer called for a refused rollback")
				}
				return
			}
			if err != nil {
				t.Fatalf("Rollback: %v", err)
			}
			if got.To.ID != want.to || got.RolledBack.ID != want.rolledBack || !slices.Equal(deploymentIDs(got.Migrations), want.crossed) {
				t.Fatalf("Rollback = to %d, rolled back %d, migrations %v; want %d, %d, %v", got.To.ID, got.RolledBack.ID, deploymentIDs(got.Migrations), want.to, want.rolledBack, want.crossed)
			}
			rb := f.deployment(got.Rollback.ID)
			if rb.Migration != (len(want.crossed) > 0) {
				t.Errorf("rollback deployment migration = %v", rb.Migration)
			}
			var detail map[string]any
			if err := json.Unmarshal(f.snapshot().audit[0].Detail, &detail); err != nil {
				t.Fatalf("audit detail: %v", err)
			}
			_, confirmed := detail["migration_confirmed"]
			if confirmed != (len(want.crossed) > 0) {
				t.Errorf("audit detail = %v", detail)
			}
		})
	}
}

func TestConfirmedRollbackRecordsTheCrossing(t *testing.T) {
	t.Parallel()

	f := newFakeRepo()
	v1 := f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
	v2 := f.add(flagged(deployment(blog, core.StateSucceeded, sha2, digestB), "trailer"))
	f.add(deployment(blog, core.StateSucceeded, sha3, digestC))
	v4 := f.add(flagged(deployment(blog, core.StateVerifying, sha4, digestD), "label"))
	svc, _ := newService(t, f)

	got, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, ToDeployment: &v1.ID, ConfirmMigration: true})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	rb := f.deployment(got.Rollback.ID)
	if !rb.Migration || rb.MigrationReason != "rolls back across deployments 2, 4" {
		t.Errorf("rollback deployment = %v %q", rb.Migration, rb.MigrationReason)
	}
	var detail map[string]any
	if err := json.Unmarshal(f.snapshot().audit[0].Detail, &detail); err != nil {
		t.Fatalf("audit detail: %v", err)
	}
	ids, _ := detail["migration_deployment_ids"].([]any)
	if detail["migration_confirmed"] != true || len(ids) != 2 || ids[0] != float64(v2.ID) || ids[1] != float64(v4.ID) {
		t.Errorf("audit detail = %v", detail)
	}
	if got.RolledBack.ID != v4.ID || got.RolledBack.State != core.StateCancelled {
		t.Errorf("RolledBack = %+v", got.RolledBack)
	}

	crossed, err := svc.MigrationsCrossed(t.Context(), blog, v1, v4)
	if err != nil || !slices.Equal(deploymentIDs(crossed), []core.DeploymentID{v2.ID, v4.ID}) {
		t.Errorf("MigrationsCrossed = %v, %v", deploymentIDs(crossed), err)
	}
}

func TestMigrationCheckKeepsEarlierErrorsFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(f *fakeRepo)
		request func(f *fakeRepo) rollback.Request
		wantErr error
	}{
		{
			name: "rollback in progress",
			setup: func(f *fakeRepo) {
				v1 := f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				f.add(flagged(deployment(blog, core.StateSucceeded, sha2, digestB), "trailer"))
				f.add(core.Deployment{Target: blog, Kind: core.KindRollback, State: core.StateQueued, Commit: core.Commit{SHA: sha1}, Digest: digestA, RollbackOfID: &v1.ID})
			},
			wantErr: core.ErrRollbackInProgress,
		},
		{
			name: "blocked target",
			setup: func(f *fakeRepo) {
				f.add(deployment(blog, core.StateSucceeded, sha1, digestA))
				f.add(flagged(deployment(blog, core.StateSucceeded, sha2, digestB), "trailer"))
				f.addBlock(core.Block{Target: blog, Digest: digestA, CreatedBy: "user:bob"})
			},
			request: func(*fakeRepo) rollback.Request { return rollback.Request{ToDeployment: idPtr(1)} },
			wantErr: core.ErrBlocked,
		},
		{
			name: "no target",
			setup: func(f *fakeRepo) {
				f.add(flagged(deployment(blog, core.StateSucceeded, sha1, digestA), "trailer"))
			},
			wantErr: core.ErrNoRollbackTarget,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRepo()
			tt.setup(f)
			svc, _ := newService(t, f)
			req := rollback.Request{}
			if tt.request != nil {
				req = tt.request(f)
			}
			req.Target, req.Actor = blog, alice
			if _, err := svc.Rollback(t.Context(), req); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Rollback error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestPostgresRollbackAcrossMigration(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	svc, err := rollback.New(rollback.Options{Repository: rollback.NewRepository(st)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v1 := pgDeployed(t, st, sha1, digestA, toSucceeded...)
	migration := pgDeployment(sha2)
	migration.Migration, migration.MigrationReason = true, "label db-migration on #7 (2222222)"
	v2, _, err := st.EnqueueDeployment(t.Context(), migration, core.PriorityNormal)
	if err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	v2.Digest = digestB
	v2 = advance(t, st, v2, toSucceeded...)
	v3 := pgDeployed(t, st, sha3, digestC, toSucceeded...)

	_, err = svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, ToDeployment: &v1.ID})
	crossing, ok := errors.AsType[*core.MigrationRollbackError](err)
	if !ok || !slices.Equal(crossing.IDs(), []core.DeploymentID{v2.ID}) || crossing.Migrations[0].MigrationReason != migration.MigrationReason {
		t.Fatalf("Rollback error = %v, want a crossing of deployment %d", err, v2.ID)
	}
	if entries, err := st.QueueEntries(t.Context(), blog.App); err != nil || len(entries) != 0 {
		t.Fatalf("queue after a refused rollback = %+v, %v", entries, err)
	}

	got, err := svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, ToDeployment: &v1.ID, ConfirmMigration: true})
	if err != nil {
		t.Fatalf("confirmed Rollback: %v", err)
	}
	if got.RolledBack.ID != v3.ID || !got.Rollback.Migration || got.Rollback.MigrationReason != fmt.Sprintf("rolls back across deployment %d", v2.ID) {
		t.Errorf("Rollback = %+v", got.Rollback)
	}
	audit, err := st.ListAudit(t.Context(), store.AuditFilter{Action: core.AuditRollbackRequest})
	if err != nil || len(audit) != 1 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	var detail map[string]any
	if err := json.Unmarshal(audit[0].Detail, &detail); err != nil || detail["migration_confirmed"] != true {
		t.Errorf("audit detail = %s, %v", audit[0].Detail, err)
	}
}

func deploymentIDs(ds []core.Deployment) []core.DeploymentID {
	out := make([]core.DeploymentID, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}
