package intake_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

type storeQueue struct {
	tx *store.Store
}

func (q storeQueue) Enqueue(ctx context.Context, d core.Deployment) (core.Deployment, error) {
	return q.tx.CreateDeployment(ctx, d, 0)
}

func queueOver(tx *store.Store) intake.Queue {
	return storeQueue{tx: tx}
}

func openStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := storetest.DatabaseURL(t)
	st, err := store.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(t.Context(), slog.New(slog.DiscardHandler), store.WithoutSessionLock()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return st, pool
}

func countRows(t *testing.T, db *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), sql).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return n
}

func TestPostgresPushToDeployment(t *testing.T) {
	t.Parallel()

	st, db := openStore(t)
	checks := newFakeChecks()
	svc, err := intake.New(intake.Options{
		Apps:    []intake.App{blogApp("staging"), blogApp("prod", "test", "e2e")},
		Commits: intake.NewPostgresCommits(st, queueOver),
		Checks:  checks,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	results, err := svc.HandlePush(t.Context(), push(shaA))
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	if len(results) != 2 || results[0].Enqueued || results[1].Enqueued {
		t.Fatalf("HandlePush = %+v, want two pending commits", results)
	}
	pending, err := svc.Pending(t.Context(), 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("Pending = %+v, %v, want 2", pending, err)
	}

	checks.set("example-user/blog", shaA, green("test"))
	results, err = svc.Reevaluate(t.Context(), "EXAMPLE-USER/BLOG", shaA)
	if err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	enqueued := 0
	for _, r := range results {
		if r.Enqueued {
			enqueued++
			if r.Commit.Target != blogApp("staging").Target {
				t.Errorf("enqueued %s, want only staging", r.Commit.Target)
			}
		}
	}
	if enqueued != 1 {
		t.Fatalf("Reevaluate = %+v, want staging enqueued", results)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := svc.Reevaluate(t.Context(), "example-user/blog", shaA); err != nil {
				t.Errorf("concurrent Reevaluate: %v", err)
			}
			if _, err := svc.HandlePush(t.Context(), push(shaA)); err != nil {
				t.Errorf("concurrent HandlePush: %v", err)
			}
		})
	}
	wg.Wait()

	if n := countRows(t, db, "SELECT count(*) FROM deployments"); n != 1 {
		t.Errorf("%d deployments, want 1", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM queue_entries"); n != 1 {
		t.Errorf("%d queue entries, want 1", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM tracked_commits t JOIN deployments d ON d.id = t.deployment_id WHERE d.commit_sha = t.commit_sha AND d.image_tag = t.commit_sha"); n != 1 {
		t.Errorf("%d tracked commits linked to their deployment, want 1", n)
	}

	if _, err := svc.HandlePush(t.Context(), push(shaB)); err != nil {
		t.Fatalf("HandlePush newer commit: %v", err)
	}
	checks.set("example-user/blog", shaA, green("test", "e2e"))
	results, err = svc.Reevaluate(t.Context(), "example-user/blog", shaA)
	if err != nil || len(results) != 0 {
		t.Errorf("Reevaluate of superseded commits = %+v, %v, want none", results, err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM deployments"); n != 1 {
		t.Errorf("superseded commit was deployed: %d deployments", n)
	}
}
