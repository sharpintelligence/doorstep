package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestOpenErrorsHidePassword(t *testing.T) {
	t.Parallel()

	const password = "hunter2-very-secret"
	tests := []struct {
		name   string
		url    string
		wantIs error
	}{
		{name: "malformed port", url: "postgres://doorstep:" + password + "@localhost:notaport/doorstep"},
		{name: "bad escape", url: "postgres://doorstep:" + password + "%zz@localhost/doorstep"},
		{name: "unknown sslmode", url: "postgres://doorstep:" + password + "@localhost/doorstep?sslmode=sometimes"},
		{name: "keyword form", url: "host=localhost password=" + password + " port=notaport"},
		{name: "unreachable", url: "postgres://doorstep:" + password + "@127.0.0.1:1/doorstep?connect_timeout=1&sslmode=disable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := store.Open(t.Context(), tt.url)
			if err == nil {
				st.Close()
				t.Fatal("Open succeeded")
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), password) {
					t.Fatalf("error chain leaks the password: %q", e)
				}
			}
			if strings.Contains(err.Error(), tt.url) {
				t.Errorf("error contains the URL: %q", err)
			}
		})
	}
}

func TestOpenRejectsMalformedURL(t *testing.T) {
	t.Parallel()

	_, err := store.Open(t.Context(), "postgres://u:p@localhost:notaport/db")
	if !errors.Is(err, store.ErrInvalidDatabaseURL) {
		t.Errorf("Open error = %v, want ErrInvalidDatabaseURL", err)
	}
}

func TestPingAfterClose(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if err := st.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	st.Close()
	if err := st.Ping(t.Context()); err == nil {
		t.Error("Ping after Close succeeded")
	}
}

func TestApplicationName(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	var name string
	if err := store.QueryRowSQL(t.Context(), st, "SELECT current_setting('application_name')").Scan(&name); err != nil {
		t.Fatalf("query application_name: %v", err)
	}
	if name != "doorstep" {
		t.Errorf("application_name = %q, want doorstep", name)
	}
}

func TestInTx(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		run     func(t *testing.T, st *store.Store) error
		wantErr error
		present []string
		absent  []string
	}{
		{
			name: "commit persists",
			run: func(t *testing.T, st *store.Store) error {
				t.Helper()
				return st.InTx(t.Context(), func(tx *store.Store) error {
					_, err := tx.EnsureApp(t.Context(), core.Target{App: "committed", Environment: "prod"})
					return err
				})
			},
			present: []string{"committed"},
		},
		{
			name: "error rolls back",
			run: func(t *testing.T, st *store.Store) error {
				t.Helper()
				return st.InTx(t.Context(), func(tx *store.Store) error {
					if _, err := tx.EnsureApp(t.Context(), core.Target{App: "discarded", Environment: "prod"}); err != nil {
						return err
					}
					return errBoom
				})
			},
			wantErr: errBoom,
			absent:  []string{"discarded"},
		},
		{
			name: "panic rolls back",
			run: func(t *testing.T, st *store.Store) (err error) {
				t.Helper()
				defer func() {
					if v := recover(); v != nil {
						err = errBoom
					}
				}()
				return st.InTx(t.Context(), func(tx *store.Store) error {
					if _, err := tx.EnsureApp(t.Context(), core.Target{App: "panicked", Environment: "prod"}); err != nil {
						return err
					}
					panic("boom")
				})
			},
			wantErr: errBoom,
			absent:  []string{"panicked"},
		},
		{
			name: "nested transaction is a savepoint",
			run: func(t *testing.T, st *store.Store) error {
				t.Helper()
				return st.InTx(t.Context(), func(tx *store.Store) error {
					if _, err := tx.EnsureApp(t.Context(), core.Target{App: "outer", Environment: "prod"}); err != nil {
						return err
					}
					inner := tx.InTx(t.Context(), func(nested *store.Store) error {
						if _, err := nested.EnsureApp(t.Context(), core.Target{App: "inner", Environment: "prod"}); err != nil {
							return err
						}
						return errBoom
					})
					if !errors.Is(inner, errBoom) {
						t.Errorf("nested InTx = %v, want %v", inner, errBoom)
					}
					_, err := tx.EnsureApp(t.Context(), core.Target{App: "after-savepoint", Environment: "prod"})
					return err
				})
			},
			present: []string{"outer", "after-savepoint"},
			absent:  []string{"inner"},
		},
		{
			name: "nested commit is visible to the outer transaction",
			run: func(t *testing.T, st *store.Store) error {
				t.Helper()
				return st.InTx(t.Context(), func(tx *store.Store) error {
					err := tx.InTx(t.Context(), func(nested *store.Store) error {
						_, err := nested.EnsureApp(t.Context(), core.Target{App: "released", Environment: "prod"})
						return err
					})
					if err != nil {
						return err
					}
					if n := countApps(t, tx, "released"); n != 1 {
						t.Errorf("outer transaction sees %d released apps, want 1", n)
					}
					return nil
				})
			},
			present: []string{"released"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			err := tt.run(t, st)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("InTx error = %v, want %v", err, tt.wantErr)
			}
			for _, name := range tt.present {
				if n := countApps(t, st, name); n != 1 {
					t.Errorf("app %q rows = %d, want 1", name, n)
				}
			}
			for _, name := range tt.absent {
				if n := countApps(t, st, name); n != 0 {
					t.Errorf("app %q rows = %d, want 0", name, n)
				}
			}
		})
	}
}

func TestTransactionStoreRejectsPoolOperations(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	err := st.InTx(t.Context(), func(tx *store.Store) error {
		if err := tx.Ping(t.Context()); err == nil {
			t.Error("Ping inside a transaction succeeded")
		}
		if _, err := tx.Migrate(t.Context(), nil); err == nil {
			t.Error("Migrate inside a transaction succeeded")
		}
		if _, _, err := tx.MigrationStatus(t.Context()); err == nil {
			t.Error("MigrationStatus inside a transaction succeeded")
		}
		tx.Close()
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if err := st.Ping(t.Context()); err != nil {
		t.Errorf("Close inside a transaction closed the pool: %v", err)
	}
}

func TestEnsureApp(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	web := core.Target{App: "web", Environment: "prod"}
	first, err := st.EnsureApp(t.Context(), web)
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	again, err := st.EnsureApp(t.Context(), web)
	if err != nil {
		t.Fatalf("EnsureApp again: %v", err)
	}
	if first != again {
		t.Errorf("EnsureApp ids differ: %d then %d", first, again)
	}
	staging, err := st.EnsureApp(t.Context(), core.Target{App: "web", Environment: "staging"})
	if err != nil {
		t.Fatalf("EnsureApp staging: %v", err)
	}
	if staging == first {
		t.Errorf("different environments share app id %d", staging)
	}
	if _, err := st.EnsureApp(t.Context(), core.Target{App: "web"}); err == nil {
		t.Error("EnsureApp accepted a target without environment")
	}
	if n := countApps(t, st, "web"); n != 2 {
		t.Errorf("web app rows = %d, want 2", n)
	}
}

func TestEnsureAppConcurrent(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	const workers = 8
	ids := make(chan store.AppID, workers)
	errs := make(chan error, workers)
	start := make(chan struct{})
	for range workers {
		go func() {
			<-start
			id, err := st.EnsureApp(t.Context(), core.Target{App: "race", Environment: "prod"})
			ids <- id
			errs <- err
		}()
	}
	close(start)
	seen := map[store.AppID]bool{}
	for range workers {
		if err := <-errs; err != nil {
			t.Errorf("EnsureApp: %v", err)
		}
		seen[<-ids] = true
	}
	if len(seen) != 1 {
		t.Errorf("concurrent EnsureApp returned ids %v, want one", seen)
	}
}

func TestCreateDeployment(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	before := time.Now().Add(-time.Minute)
	in := newDeployment("web")
	in.Digest = testDigest
	in.PreviousDigest = testDigest
	got := mustCreate(t, st, in, 5)

	if got.ID <= 0 {
		t.Errorf("ID = %d, want positive", got.ID)
	}
	if got.State != core.StateQueued {
		t.Errorf("State = %s, want queued", got.State)
	}
	if got.CreatedAt.Before(before) || got.UpdatedAt.Before(before) {
		t.Errorf("timestamps not set: created %v updated %v", got.CreatedAt, got.UpdatedAt)
	}
	want := in
	want.ID = got.ID
	want.State = core.StateQueued
	want.CreatedAt = got.CreatedAt
	want.UpdatedAt = got.UpdatedAt
	assertDeployment(t, got, want)

	if n := queryInt(t, st, "SELECT priority FROM queue_entries WHERE deployment_id = $1 AND lease_owner IS NULL AND attempts = 0", got.ID); n != 5 {
		t.Errorf("queue priority = %d, want 5", n)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployment_events WHERE deployment_id = $1 AND type = 'transition' AND from_state IS NULL AND to_state = 'queued' AND actor = 'octocat'", got.ID); n != 1 {
		t.Errorf("queued events = %d, want 1", n)
	}

	fetched, err := st.GetDeployment(t.Context(), got.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	assertDeployment(t, fetched, got)

	second := mustCreate(t, st, newDeployment("web"), 0)
	if second.ID <= got.ID {
		t.Errorf("second ID %d is not after %d", second.ID, got.ID)
	}
	if n := countApps(t, st, "web"); n != 1 {
		t.Errorf("web app rows = %d, want 1", n)
	}
}

func TestCreateDeploymentLineage(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	original := mustCreate(t, st, newDeployment("web"), 0)

	retry := newDeployment("web")
	retry.ParentID = &original.ID
	retried := mustCreate(t, st, retry, 0)
	if retried.ParentID == nil || *retried.ParentID != original.ID {
		t.Errorf("ParentID = %v, want %d", retried.ParentID, original.ID)
	}

	rb := newDeployment("web")
	rb.Kind = core.KindRollback
	rb.Source = core.SourceSystem
	rb.RollbackOfID = &original.ID
	rb.Digest = testDigest
	rolled := mustCreate(t, st, rb, 100)
	if rolled.Kind != core.KindRollback || rolled.RollbackOfID == nil || *rolled.RollbackOfID != original.ID {
		t.Errorf("rollback = %+v", rolled)
	}

	mustExec(t, st, "DELETE FROM deployments WHERE id = $1", original.ID)
	after, err := st.GetDeployment(t.Context(), rolled.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if after.RollbackOfID != nil {
		t.Errorf("RollbackOfID after delete = %d, want nil", *after.RollbackOfID)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM queue_entries WHERE deployment_id = $1", original.ID); n != 0 {
		t.Errorf("queue entries for deleted deployment = %d, want 0", n)
	}
}

func TestCreateDeploymentRejects(t *testing.T) {
	t.Parallel()

	missing := core.DeploymentID(1 << 40)
	tests := []struct {
		name   string
		mutate func(*core.Deployment)
		want   string
	}{
		{name: "short commit", mutate: func(d *core.Deployment) { d.Commit.SHA = "abc1234" }, want: "commit SHA"},
		{name: "tag as digest", mutate: func(d *core.Deployment) { d.Digest = "latest" }, want: "digest"},
		{name: "unknown source", mutate: func(d *core.Deployment) { d.Source = "cron" }, want: "source"},
		{name: "not queued", mutate: func(d *core.Deployment) { d.State = core.StateSyncing }, want: "start queued"},
		{name: "already superseded", mutate: func(d *core.Deployment) { d.SupersededByID = &missing }, want: "superseded"},
		{name: "missing parent", mutate: func(d *core.Deployment) { d.ParentID = &missing }, want: "deployments_parent_id_fkey"},
		{name: "missing rollback origin", mutate: func(d *core.Deployment) {
			d.Kind = core.KindRollback
			d.RollbackOfID = &missing
		}, want: "deployments_rollback_of_id_fkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := newDeployment("web")
			tt.mutate(&d)
			_, err := st.CreateDeployment(t.Context(), d, 0)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CreateDeployment error = %v, want it to mention %q", err, tt.want)
			}
			for _, table := range []string{"apps", "deployments", "queue_entries", "deployment_events"} {
				if n := queryInt(t, st, "SELECT count(*) FROM "+table); n != 0 {
					t.Errorf("%s has %d rows after a rejected create, want 0", table, n)
				}
			}
		})
	}
}

func TestGetDeploymentNotFound(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	_, err := st.GetDeployment(t.Context(), 42)
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetDeployment error = %v, want core.ErrNotFound", err)
	}
}

func TestGetDeploymentRoundTripsOptionalColumns(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	created := mustCreate(t, st, newDeployment("web"), 0)
	successor := mustCreate(t, st, newDeployment("web"), 0)
	mustExec(t, st, `UPDATE deployments
		SET state = 'superseded', pin_commit_sha = $2, previous_digest = $3, superseded_by_id = $4,
		    failure_reason = 'replaced', started_at = now(), finished_at = now(),
		    gitops_repository = 'acme/gitops', argocd_revision = '1.2.3'
		WHERE id = $1`, created.ID, testSHA, testDigest, successor.ID)

	got, err := st.GetDeployment(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.State != core.StateSuperseded || got.PinCommitSHA != testSHA || got.PreviousDigest != testDigest ||
		got.FailureReason != "replaced" || got.SupersededByID == nil || *got.SupersededByID != successor.ID ||
		got.StartedAt == nil || got.FinishedAt == nil || got.Digest != "" ||
		got.GitOpsRepository != "acme/gitops" || got.ArgoCDRevision != "1.2.3" {
		t.Errorf("GetDeployment = %+v", got)
	}
}

func assertDeployment(t *testing.T, got, want core.Deployment) {
	t.Helper()
	if got.ID != want.ID || got.Target != want.Target || got.Kind != want.Kind || got.State != want.State ||
		got.Source != want.Source || got.RequestedBy != want.RequestedBy || got.Commit != want.Commit ||
		got.Image != want.Image || got.Digest != want.Digest || got.PreviousDigest != want.PreviousDigest ||
		got.PinCommitSHA != want.PinCommitSHA || got.FailureReason != want.FailureReason ||
		got.GitOpsRepository != want.GitOpsRepository || got.ArgoCDRevision != want.ArgoCDRevision ||
		!equalID(got.ParentID, want.ParentID) || !equalID(got.RollbackOfID, want.RollbackOfID) ||
		!equalID(got.SupersededByID, want.SupersededByID) ||
		!got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) ||
		!equalTime(got.StartedAt, want.StartedAt) || !equalTime(got.FinishedAt, want.FinishedAt) {
		t.Errorf("deployment mismatch\n got  %+v\n want %+v", got, want)
	}
}

func equalID(a, b *core.DeploymentID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
