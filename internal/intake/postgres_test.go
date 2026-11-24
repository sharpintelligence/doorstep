package intake_test

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/queue"
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

type realQueue struct {
	queue *queue.Queue
}

func (q realQueue) Enqueue(ctx context.Context, d core.Deployment) (core.Deployment, error) {
	result, err := q.queue.Enqueue(ctx, d, queue.EnqueueOptions{})
	return result.Deployment, err
}

func realQueueOver(tx *store.Store) intake.Queue {
	return realQueue{queue: queue.New(queue.Options{Store: tx})}
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
	branches := newFakeBranches()
	branches.set("example-user/blog", "main", shaA)
	svc, err := intake.New(intake.Options{
		Apps:     []intake.App{blogApp("staging"), blogApp("prod", "test", "e2e")},
		Commits:  intake.NewPostgresCommits(st, queueOver),
		Checks:   checks,
		Branches: branches,
		Now:      newTickingClock().Now,
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
	open, err := svc.Open(t.Context(), 10)
	if err != nil || len(open) != 2 {
		t.Fatalf("Open = %+v, %v, want 2", open, err)
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

	branches.set("example-user/blog", "main", shaB)
	if _, err := svc.HandlePush(t.Context(), push(shaB)); err != nil {
		t.Fatalf("HandlePush newer commit: %v", err)
	}
	checks.set("example-user/blog", shaA, green("test", "e2e"))
	results, err = svc.Reevaluate(t.Context(), "example-user/blog", shaA)
	if err != nil || len(results) != 0 {
		t.Errorf("Reevaluate of superseded commits = %+v, %v, want none", results, err)
	}
	if results, err := svc.HandlePush(t.Context(), push(shaA)); err != nil || len(results) != 0 {
		t.Errorf("late redelivery of the older push = %+v, %v, want none", results, err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM deployments"); n != 1 {
		t.Errorf("superseded commit was deployed: %d deployments", n)
	}
	open, err = svc.Open(t.Context(), 10)
	if err != nil || len(open) != 2 || open[0].Commit.SHA != shaB || open[1].Commit.SHA != shaB {
		t.Errorf("Open = %+v, %v, want the head for both apps", open, err)
	}
}

func TestPostgresOlderSnapshotNeverOverridesNewerOne(t *testing.T) {
	t.Parallel()

	st, db := openStore(t)
	checks := newFakeChecks()
	branches := newFakeBranches()
	branches.set("example-user/blog", "main", shaA)
	svc, err := intake.New(intake.Options{
		Apps:     []intake.App{blogApp("prod", "test")},
		Commits:  intake.NewPostgresCommits(st, queueOver),
		Checks:   checks,
		Branches: branches,
		Now:      newTickingClock().Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := svc.HandlePush(t.Context(), push(shaA)); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	olderRead := make(chan struct{})
	newerSettled := make(chan struct{})
	checks.onRead = func(call int, s intake.Snapshot) intake.Snapshot {
		switch call {
		case 2:
			close(olderRead)
			<-newerSettled
			return green("test")
		case 3:
			return intake.Snapshot{CheckRuns: []github.CheckRun{run(2, "test", "in_progress", "")}}
		}
		return s
	}
	var (
		wg       sync.WaitGroup
		olderErr error
	)
	wg.Go(func() { _, olderErr = svc.Reevaluate(t.Context(), "example-user/blog", shaA) })
	<-olderRead
	if _, err := svc.Reevaluate(t.Context(), "example-user/blog", shaA); err != nil {
		t.Fatalf("newer Reevaluate: %v", err)
	}
	close(newerSettled)
	wg.Wait()
	if olderErr != nil {
		t.Fatalf("older Reevaluate: %v", olderErr)
	}

	if n := countRows(t, db, "SELECT count(*) FROM deployments"); n != 0 {
		t.Errorf("a stale green snapshot enqueued %d deployments after a re-run started", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM tracked_commits WHERE checks_state = 'pending'"); n != 1 {
		t.Errorf("%d pending tracked commits, want the newer pending snapshot saved", n)
	}
}

func TestPostgresWebhookAndPollerEndToEnd(t *testing.T) {
	t.Parallel()

	st, db := openStore(t)
	checks := newFakeChecks()
	branches := newFakeBranches()
	branches.set("example-user/blog", "main", shaA)
	svc, err := intake.New(intake.Options{
		Apps:     []intake.App{blogApp("prod", "test")},
		Commits:  intake.NewPostgresCommits(st, queueOver),
		Checks:   checks,
		Branches: branches,
		Now:      newTickingClock().Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	deliveries := intake.NewPostgresDeliveries(st)
	handler, err := intake.NewWebhook(intake.WebhookOptions{
		Secret:        []byte(webhookSecret),
		Intake:        svc,
		Deliveries:    deliveries,
		Installations: &fakeInstallations{},
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	send := func(d delivery) string {
		t.Helper()
		code, body := serve(t, handler, d.request(t.Context()))
		if code != http.StatusOK {
			t.Fatalf("%s delivery %s = %d %v", d.event, d.id, code, body)
		}
		return body["status"]
	}

	pushed := delivery{event: "push", id: "push-1", body: pushBody("refs/heads/main", shaA, false, "octocat", "")}
	if got := send(pushed); got != "processed" {
		t.Errorf("push = %s, want processed", got)
	}
	if got := send(pushed); got != "duplicate" {
		t.Errorf("redelivered push = %s, want duplicate", got)
	}
	if n := countRows(t, db, "SELECT count(*) FROM tracked_commits WHERE source = 'webhook' AND pushed_by = 'octocat'"); n != 1 {
		t.Errorf("%d tracked commits from the webhook, want 1", n)
	}

	checks.set("example-user/blog", shaA, green("test"))
	status := delivery{event: "status", id: "status-1", body: `{"sha":"` + shaA + `","repository":{"full_name":"example-user/blog"}}`}
	if got := send(status); got != "processed" {
		t.Errorf("status = %s, want processed", got)
	}
	if n := countRows(t, db, "SELECT count(*) FROM deployments WHERE source = 'webhook'"); n != 1 {
		t.Errorf("%d deployments, want 1", n)
	}
	if n := countRows(t, db, "SELECT count(*) FROM webhook_deliveries WHERE completed_at IS NOT NULL"); n != 1 {
		t.Errorf("%d completed deliveries, want only the push", n)
	}

	branches.set("example-user/blog", "main", shaB)
	checks.set("example-user/blog", shaB, green("test"))
	readsBefore := branches.calls
	replicas := make([]*intake.Poller, 3)
	for i := range replicas {
		replicas[i], err = intake.NewPoller(intake.PollerOptions{
			Apps:     []intake.App{blogApp("prod", "test")},
			Intake:   svc,
			Branches: branches,
			Claims:   st,
			Interval: time.Hour,
		})
		if err != nil {
			t.Fatalf("NewPoller: %v", err)
		}
	}
	var wg sync.WaitGroup
	for _, p := range replicas {
		wg.Go(func() {
			if err := p.RunOnce(t.Context()); err != nil {
				t.Errorf("RunOnce: %v", err)
			}
		})
	}
	wg.Wait()
	if n := countRows(t, db, "SELECT count(*) FROM deployments WHERE source = 'poll' AND requested_by = 'doorstep-poller'"); n != 1 {
		t.Errorf("%d polled deployments, want 1", n)
	}
	if n := branches.calls - readsBefore; n != 2 {
		t.Errorf("branch reads = %d, want one poll cycle (head read plus Track)", n)
	}

	readsBefore = branches.calls
	if err := replicas[0].RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce right after a cycle: %v", err)
	}
	if n := branches.calls - readsBefore; n != 0 {
		t.Errorf("branch reads = %d right after a cycle, want none until the interval passes", n)
	}
	mustExecPool(t, db, "UPDATE intake_poll_state SET next_poll_at = now()")
	if err := replicas[2].RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce once due: %v", err)
	}
	if n := branches.calls - readsBefore; n != 1 {
		t.Errorf("branch reads = %d, want only the head read for a head intake already tracks", n)
	}

	stranded := delivery{event: "push", id: "push-2", body: pushBody("refs/heads/main", shaA, false, "octocat", "")}
	mustExecPool(t, db, "INSERT INTO webhook_deliveries (delivery_id, event, claimed_at) VALUES ('push-2', 'push', now() - interval '1 minute')")
	if got := send(stranded); got != "processed" {
		t.Errorf("redelivery of a stranded claim = %s, want processed", got)
	}
	if got := send(stranded); got != "duplicate" {
		t.Errorf("second redelivery = %s, want duplicate", got)
	}

	mustExecPool(t, db, "UPDATE webhook_deliveries SET claimed_at = now() - interval '8 days', completed_at = now() - interval '8 days' WHERE delivery_id = 'push-1'")
	deleted, err := deliveries.DeleteBefore(t.Context(), intake.DeliveryRetention)
	if err != nil || deleted != 1 {
		t.Errorf("DeleteBefore = %d, %v, want 1", deleted, err)
	}
	if got := send(pushed); got != "processed" {
		t.Errorf("push after retention = %s, want processed", got)
	}
}

func TestPostgresBatchedReleaseKeepsSupersededMigrationFlag(t *testing.T) {
	t.Parallel()

	st, _ := openStore(t)
	checks := newFakeChecks()
	branches := newFakeBranches()
	source := newFakeMigrations()
	source.commits[shaMid] = github.CommitInfo{SHA: shaMid, Message: "add column\n\nDoorstep-Migration: true"}
	source.ranges[shaMid+"..."+shaA] = github.CommitRange{Status: github.ComparisonAhead, Total: 1, Commits: []github.CommitInfo{{SHA: shaA, Message: "fix typo"}}}
	app := migratingApp("prod", intake.MigrationPolicy{Trailer: true})
	svc, err := intake.New(intake.Options{
		Apps:       []intake.App{app},
		Commits:    intake.NewPostgresCommits(st, realQueueOver),
		Checks:     checks,
		Branches:   branches,
		Migrations: source,
		Baselines:  st,
		Now:        newTickingClock().Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, sha := range []string{shaMid, shaA} {
		branches.set("example-user/blog", "main", sha)
		checks.set("example-user/blog", sha, green("test"))
		results, err := svc.HandlePush(t.Context(), push(sha))
		if err != nil || len(results) != 1 || !results[0].Enqueued {
			t.Fatalf("HandlePush(%s) = %+v, %v, want enqueued", sha[:4], results, err)
		}
	}

	q := queue.New(queue.Options{Store: st, Lease: time.Minute, MaxBatch: func(string) int { return 5 }})
	claimed, ok, err := q.Claim(t.Context(), "worker-1")
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v", ok, err)
	}
	if claimed.Deployment.Commit.SHA != shaA || len(claimed.Superseded) != 1 {
		t.Fatalf("claimed %s superseding %v, want the newest commit superseding the flagged one", claimed.Deployment.Commit.SHA[:4], claimed.Superseded)
	}
	want := "unreleased migration in deployment " + strconv.FormatInt(int64(claimed.Superseded[0]), 10)
	if !claimed.Deployment.Migration || claimed.Deployment.MigrationReason != want {
		t.Errorf("winner migration = %v %q, want true %q", claimed.Deployment.Migration, claimed.Deployment.MigrationReason, want)
	}
}

func mustExecPool(t *testing.T, db *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := db.Exec(t.Context(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
