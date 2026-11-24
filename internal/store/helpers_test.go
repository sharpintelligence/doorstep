package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const (
	testSHA     = "0123456789abcdef0123456789abcdef01234567"
	otherSHA    = "89abcdef0123456789abcdef0123456789abcdef"
	testDigest  = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	otherDigest = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func newDeployment(app string) core.Deployment {
	return core.Deployment{
		Target:      core.Target{App: app, Environment: "prod"},
		Kind:        core.KindDeploy,
		Source:      core.SourceWebhook,
		RequestedBy: "octocat",
		Commit:      core.Commit{Repository: "acme/" + app, SHA: testSHA},
		Image:       core.ImageRef{Repository: "ghcr.io/acme/" + app, Tag: testSHA},
	}
}

func randomSHA(t *testing.T) string {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random SHA: %v", err)
	}
	return hex.EncodeToString(b)
}

func deploymentOf(app, sha string) core.Deployment {
	d := newDeployment(app)
	d.Commit.SHA = sha
	d.Image.Tag = sha
	return d
}

func openMigrated(t *testing.T) (*store.Store, string) {
	t.Helper()
	databaseURL := storetest.DatabaseURL(t)
	st := openUnmigrated(t, databaseURL)
	if _, err := st.Migrate(t.Context(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st, databaseURL
}

func schemaOf(t *testing.T, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	schema := u.Query().Get("search_path")
	if schema == "" {
		t.Fatalf("database URL has no search_path")
	}
	return schema
}

func connectRaw(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	query := u.Query()
	query.Del("pool_max_conns")
	u.RawQuery = query.Encode()
	conn, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

func mustCreate(t *testing.T, st *store.Store, d core.Deployment, priority int) core.Deployment {
	t.Helper()
	created, err := st.CreateDeployment(t.Context(), d, priority)
	if err != nil {
		t.Fatalf("CreateDeployment(%s): %v", d.Target, err)
	}
	return created
}

func pathTo(to core.State) []core.State {
	paths := map[core.State][]core.State{
		core.StateQueued:     {},
		core.StateResolving:  {core.StateResolving},
		core.StatePinning:    {core.StateResolving, core.StatePinning},
		core.StateSyncing:    {core.StateResolving, core.StatePinning, core.StateSyncing},
		core.StateVerifying:  {core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying},
		core.StateSucceeded:  {core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying, core.StateSucceeded},
		core.StateFailed:     {core.StateResolving, core.StateFailed},
		core.StateCancelled:  {core.StateCancelled},
		core.StateSuperseded: {core.StateSuperseded},
	}
	return paths[to]
}

func mustAdvance(t *testing.T, st *store.Store, d core.Deployment, to core.State) core.Deployment {
	t.Helper()
	for _, next := range pathTo(to)[indexAfter(pathTo(to), d.State):] {
		tr := withOutcome(t, st, store.Transition{ID: d.ID, From: d.State, To: next, Actor: "worker"}, d.Target.App)
		var err error
		d, err = st.TransitionDeployment(t.Context(), tr)
		if err != nil {
			t.Fatalf("TransitionDeployment(%d, %s -> %s): %v", d.ID, d.State, next, err)
		}
	}
	if d.State != to {
		t.Fatalf("deployment %d is %s after advancing, want %s", d.ID, d.State, to)
	}
	return d
}

func withOutcome(t *testing.T, st *store.Store, tr store.Transition, app string) store.Transition {
	t.Helper()
	if tr.To == core.StateFailed {
		tr.FailureReason = "test failure"
	}
	if tr.To == core.StateSuperseded {
		successor := mustCreate(t, st, newDeployment(app+"-successor"), 0)
		tr.SupersededByID = &successor.ID
	}
	return tr
}

func indexAfter(path []core.State, current core.State) int {
	for i, s := range path {
		if s == current {
			return i + 1
		}
	}
	return 0
}

func mustCreateIn(t *testing.T, st *store.Store, d core.Deployment, to core.State) core.Deployment {
	t.Helper()
	return mustAdvance(t, st, mustCreate(t, st, d, 0), to)
}

func mustExec(t *testing.T, st *store.Store, sql string, args ...any) {
	t.Helper()
	if err := store.ExecSQL(t.Context(), st, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func queryInt(t *testing.T, st *store.Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := store.QueryRowSQL(t.Context(), st, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func countApps(t *testing.T, st *store.Store, name string) int {
	t.Helper()
	return queryInt(t, st, "SELECT count(*) FROM apps WHERE name = $1", name)
}

func assertConstraint(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v is not a Postgres error, want constraint %s", err, constraint)
	}
	if pgErr.ConstraintName != constraint {
		t.Errorf("violated constraint %q (%s), want %q", pgErr.ConstraintName, pgErr.Message, constraint)
	}
}
