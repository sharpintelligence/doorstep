package rollback_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const lockWait = 300 * time.Millisecond

type gate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) pass(ctx context.Context) error {
	first := false
	g.once.Do(func() { first = true })
	if !first {
		return nil
	}
	close(g.entered)
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type gatedQueue struct {
	queueOver
	gate *gate
}

func (q gatedQueue) Enqueue(ctx context.Context, d core.Deployment) (core.Deployment, error) {
	created, err := q.queueOver.Enqueue(ctx, d)
	if err != nil {
		return created, err
	}
	return created, q.gate.pass(ctx)
}

type gatedBlocks struct {
	rollback.Repository
	gate *gate
}

func (r gatedBlocks) InTx(ctx context.Context, fn func(rollback.Repository) error) error {
	return r.Repository.InTx(ctx, func(tx rollback.Repository) error {
		return fn(gatedBlocks{tx, r.gate})
	})
}

func (r gatedBlocks) CreateBlock(ctx context.Context, b core.Block) (core.Block, bool, error) {
	created, isNew, err := r.Repository.CreateBlock(ctx, b)
	if err != nil {
		return created, isNew, err
	}
	return created, isNew, r.gate.pass(ctx)
}

type raceFixture struct {
	st     *store.Store
	bad    core.Deployment
	commit core.Commit
}

func newRaceFixture(t *testing.T) raceFixture {
	t.Helper()
	st := storetest.Open(t)
	pgDeployed(t, st, sha1, digestA, toSucceeded...)
	bad := pgDeployed(t, st, sha2, digestB, toSucceeded...)
	commit := core.Commit{Repository: "acme/blog", SHA: sha2}
	_, _, err := st.TrackCommit(t.Context(), core.TrackedCommit{
		Target: blog, Commit: commit, Branch: "main", Source: core.SourcePoll, PushedBy: "octocat",
	}, func(context.Context) (bool, error) { return true, nil })
	if err != nil {
		t.Fatalf("TrackCommit: %v", err)
	}
	return raceFixture{st: st, bad: bad, commit: commit}
}

func (f raceFixture) intake(t *testing.T, g *gate) *intake.Service {
	t.Helper()
	queueFor := func(tx *store.Store) intake.Queue { return queueOver{tx} }
	if g != nil {
		queueFor = func(tx *store.Store) intake.Queue { return gatedQueue{queueOver{tx}, g} }
	}
	in, err := intake.New(intake.Options{
		Apps: []intake.App{{
			Target: blog, Repository: "acme/blog", Branch: "main", ImageRepository: "ghcr.io/acme/blog",
		}},
		Commits:  intake.NewPostgresCommits(f.st, queueFor),
		Checks:   staticChecks{},
		Branches: staticHead(sha2),
	})
	if err != nil {
		t.Fatalf("intake.New: %v", err)
	}
	return in
}

func (f raceFixture) rollbacks(t *testing.T, g *gate) *rollback.Service {
	t.Helper()
	repo := rollback.NewRepository(f.st)
	if g != nil {
		repo = gatedBlocks{repo, g}
	}
	svc, err := rollback.New(rollback.Options{Repository: repo})
	if err != nil {
		t.Fatalf("rollback.New: %v", err)
	}
	return svc
}

func waitEntered(t *testing.T, g *gate, failed <-chan struct{}) {
	t.Helper()
	select {
	case <-g.entered:
	case <-failed:
		t.Fatal("the gated call returned before reaching the gate")
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the gated call")
	}
}

func assertWaiting(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s finished while the other transaction held the app lock", what)
	case <-time.After(lockWait):
	}
}

func TestPostgresRollbackCancelsDeployQueuedByConcurrentSettle(t *testing.T) {
	t.Parallel()

	f := newRaceFixture(t)
	g := newGate()
	in := f.intake(t, g)
	svc := f.rollbacks(t, nil)

	var (
		results   []intake.Result
		intakeErr error
	)
	intakeDone := make(chan struct{})
	go func() {
		defer close(intakeDone)
		results, intakeErr = in.Reevaluate(t.Context(), f.commit.Repository, f.commit.SHA)
	}()
	waitEntered(t, g, intakeDone)

	var (
		got         rollback.Result
		rollbackErr error
	)
	rollbackDone := make(chan struct{})
	go func() {
		defer close(rollbackDone)
		got, rollbackErr = svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, Role: core.RoleDeployer})
	}()
	assertWaiting(t, rollbackDone, "the rollback")
	close(g.release)
	<-intakeDone
	<-rollbackDone

	if intakeErr != nil || len(results) != 1 || !results[0].Enqueued {
		t.Fatalf("Reevaluate = %+v, %v, want the commit enqueued before the rollback", results, intakeErr)
	}
	if rollbackErr != nil {
		t.Fatalf("Rollback: %v", rollbackErr)
	}
	queued := *results[0].Commit.DeploymentID
	if !slices.Contains(got.Cancelled, queued) {
		t.Errorf("Cancelled = %v, want the deploy %d intake queued", got.Cancelled, queued)
	}
	d, err := f.st.GetDeployment(t.Context(), queued)
	if err != nil || d.State != core.StateCancelled {
		t.Errorf("deploy %d = %s, %v, want cancelled", queued, d.State, err)
	}
}

func TestPostgresSettleWaitsForConcurrentRollback(t *testing.T) {
	t.Parallel()

	f := newRaceFixture(t)
	g := newGate()
	in := f.intake(t, nil)
	svc := f.rollbacks(t, g)

	var rollbackErr error
	rollbackDone := make(chan struct{})
	go func() {
		defer close(rollbackDone)
		_, rollbackErr = svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, Role: core.RoleDeployer})
	}()
	waitEntered(t, g, rollbackDone)

	var (
		results   []intake.Result
		intakeErr error
	)
	intakeDone := make(chan struct{})
	go func() {
		defer close(intakeDone)
		results, intakeErr = in.Reevaluate(t.Context(), f.commit.Repository, f.commit.SHA)
	}()
	assertWaiting(t, intakeDone, "settling the commit")
	close(g.release)
	<-rollbackDone
	<-intakeDone

	if rollbackErr != nil {
		t.Fatalf("Rollback: %v", rollbackErr)
	}
	if intakeErr != nil || len(results) != 1 || results[0].Enqueued || results[0].Blocked == nil {
		t.Fatalf("Reevaluate = %+v, %v, want the commit refused as blocked", results, intakeErr)
	}
	page, err := f.st.ListDeployments(t.Context(), store.DeploymentFilter{App: blog.App, Kinds: []core.Kind{core.KindDeploy}, States: []core.State{core.StateQueued}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("queued deploys = %+v, %v, want none", page.Items, err)
	}
}

func TestPostgresConcurrentRollbacks(t *testing.T) {
	t.Parallel()

	f := newRaceFixture(t)
	svc := f.rollbacks(t, nil)

	const callers = 4
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-start
			_, errs[i] = svc.Rollback(t.Context(), rollback.Request{Target: blog, Actor: alice, Role: core.RoleDeployer})
		})
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, core.ErrRollbackInProgress):
		default:
			t.Errorf("Rollback error = %v, want success or ErrRollbackInProgress", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d rollbacks succeeded, want exactly 1: %v", succeeded, errs)
	}
	page, err := f.st.ListDeployments(t.Context(), store.DeploymentFilter{App: blog.App, Kinds: []core.Kind{core.KindRollback}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("rollback deployments = %+v, %v, want exactly one", page.Items, err)
	}
	blocks, err := f.st.ListBlocks(t.Context(), blog, false)
	if err != nil || len(blocks) != 1 || blocks[0].Digest != f.bad.Digest {
		t.Fatalf("blocks = %+v, %v, want one block of %s", blocks, err, f.bad.Digest)
	}
}
