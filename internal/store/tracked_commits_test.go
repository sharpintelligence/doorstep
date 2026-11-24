package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

var evaluatedAt = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func shaOf(c byte) string {
	return strings.Repeat(string(c), 40)
}

func newTrackedCommit(app, sha string) core.TrackedCommit {
	return core.TrackedCommit{
		Target:   core.Target{App: app, Environment: "prod"},
		Commit:   core.Commit{Repository: "Acme/" + app, SHA: sha},
		Branch:   "main",
		Source:   core.SourceWebhook,
		PushedBy: "octocat",
	}
}

func isHead(head bool) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return head, nil }
}

func mustTrack(t *testing.T, st *store.Store, c core.TrackedCommit) core.TrackedCommit {
	t.Helper()
	tracked, _, err := st.TrackCommit(t.Context(), c, isHead(true))
	if err != nil {
		t.Fatalf("TrackCommit(%s): %v", c.Commit.SHA, err)
	}
	return tracked
}

func mustGetTracked(t *testing.T, st *store.Store, id core.TrackedCommitID) core.TrackedCommit {
	t.Helper()
	c, err := st.GetTrackedCommit(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTrackedCommit(%d): %v", id, err)
	}
	return c
}

func evaluation(state core.ChecksState, readAt time.Time, checks ...core.Check) core.ChecksEvaluation {
	return core.ChecksEvaluation{State: state, Checks: checks, ReadAt: readAt}
}

func enqueueDeployment(t *testing.T, c core.TrackedCommit) func(tx *store.Store) (core.DeploymentID, error) {
	t.Helper()
	return func(tx *store.Store) (core.DeploymentID, error) {
		d := newDeployment(c.Target.App)
		d.Commit = c.Commit
		d.Image.Tag = c.Commit.SHA
		created, err := tx.CreateDeployment(t.Context(), d, 0)
		return created.ID, err
	}
}

func refuseEnqueue(t *testing.T) func(*store.Store) (core.DeploymentID, error) {
	t.Helper()
	return func(*store.Store) (core.DeploymentID, error) {
		t.Error("enqueue called for an ineligible commit")
		return 0, errors.New("enqueue refused")
	}
}

func mustSettle(t *testing.T, st *store.Store, c core.TrackedCommit, eval core.ChecksEvaluation) (core.TrackedCommit, bool) {
	t.Helper()
	settled, promoted, err := st.SettleTrackedCommit(t.Context(), c.ID, eval, enqueueDeployment(t, c))
	if err != nil {
		t.Fatalf("SettleTrackedCommit(%d): %v", c.ID, err)
	}
	return settled, promoted
}

func mustEnqueueCommit(t *testing.T, st *store.Store, c core.TrackedCommit) core.TrackedCommit {
	t.Helper()
	settled, promoted := mustSettle(t, st, c, evaluation(core.ChecksSuccess, evaluatedAt))
	if !promoted {
		t.Fatalf("SettleTrackedCommit(%d) did not enqueue: %+v", c.ID, settled)
	}
	return settled
}

func setChecksState(t *testing.T, st *store.Store, id core.TrackedCommitID, state core.ChecksState, at time.Time) {
	t.Helper()
	mustExec(t, st, "UPDATE tracked_commits SET checks_state = $2, evaluated_at = $3 WHERE id = $1", id, state, at)
}

func trackedIDs(commits []core.TrackedCommit) []core.TrackedCommitID {
	ids := make([]core.TrackedCommitID, 0, len(commits))
	for _, c := range commits {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestTrackCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	in := newTrackedCommit("blog", shaOf('a'))
	got, outcome, err := st.TrackCommit(t.Context(), in, isHead(true))
	if err != nil {
		t.Fatalf("TrackCommit: %v", err)
	}
	if outcome != core.TrackCreated {
		t.Errorf("outcome = %s for a new commit, want %s", outcome, core.TrackCreated)
	}
	if got.ID == 0 || got.Target != in.Target || got.Commit != in.Commit || got.Branch != "main" ||
		got.Source != core.SourceWebhook || got.PushedBy != "octocat" || got.ChecksState != core.ChecksPending {
		t.Errorf("TrackCommit = %+v", got)
	}
	if len(got.Checks) != 0 || got.EvaluatedAt != nil || got.EnqueuedAt != nil || !got.Open() || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("new tracked commit is not empty and open: %+v", got)
	}

	again := in
	again.Source = core.SourcePoll
	again.PushedBy = "someone-else"
	repeat, outcome, err := st.TrackCommit(t.Context(), again, isHead(true))
	if err != nil {
		t.Fatalf("second TrackCommit: %v", err)
	}
	if outcome != core.TrackExisting {
		t.Errorf("outcome = %s for a known commit, want %s", outcome, core.TrackExisting)
	}
	if repeat.ID != got.ID || repeat.Source != core.SourceWebhook || repeat.PushedBy != "octocat" || !repeat.Open() {
		t.Errorf("second TrackCommit = %+v, want the original open row %+v", repeat, got)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM tracked_commits"); n != 1 {
		t.Errorf("tracked_commits has %d rows, want 1", n)
	}
}

func TestTrackCommitRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	deployment := core.DeploymentID(1)
	newer := core.TrackedCommitID(1)
	enqueuedAt := evaluatedAt
	tests := []struct {
		name    string
		mutate  func(*core.TrackedCommit)
		head    func(context.Context) (bool, error)
		wantErr string
		wantIs  error
	}{
		{name: "short SHA", mutate: func(c *core.TrackedCommit) { c.Commit.SHA = "abc" }, wantIs: core.ErrInvalidCommitSHA},
		{name: "missing branch", mutate: func(c *core.TrackedCommit) { c.Branch = "" }, wantErr: "branch"},
		{name: "unknown source", mutate: func(c *core.TrackedCommit) { c.Source = "cron" }, wantErr: "source"},
		{name: "already evaluated", mutate: func(c *core.TrackedCommit) { c.ChecksState = core.ChecksSuccess }, wantErr: "start pending"},
		{name: "already linked", mutate: func(c *core.TrackedCommit) { c.DeploymentID = &deployment }, wantErr: "already be enqueued"},
		{name: "already enqueued", mutate: func(c *core.TrackedCommit) { c.EnqueuedAt = &enqueuedAt }, wantErr: "already be enqueued"},
		{name: "already superseded", mutate: func(c *core.TrackedCommit) { c.SupersededByID = &newer }, wantErr: "superseded"},
		{name: "no head check", head: nil, wantErr: "branch head check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			c := newTrackedCommit("blog", shaOf('a'))
			head := isHead(true)
			if tt.mutate != nil {
				tt.mutate(&c)
			} else {
				head = tt.head
			}
			_, _, err := st.TrackCommit(t.Context(), c, head)
			if err == nil {
				t.Fatal("TrackCommit accepted invalid input")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tt.wantIs)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM tracked_commits"); n != 0 {
				t.Errorf("tracked_commits has %d rows after rejected input", n)
			}
		})
	}
}

func TestTrackCommitSupersedesOtherOpenCommits(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	enqueued := mustEnqueueCommit(t, st, mustTrack(t, st, newTrackedCommit("blog", shaOf('a'))))
	failed := mustTrack(t, st, newTrackedCommit("blog", shaOf('b')))
	mustSettle(t, st, failed, evaluation(core.ChecksFailure, evaluatedAt))
	pending := mustTrack(t, st, newTrackedCommit("blog", shaOf('c')))
	other := mustTrack(t, st, newTrackedCommit("api", shaOf('d')))

	head := mustTrack(t, st, newTrackedCommit("blog", shaOf('e')))

	tests := []struct {
		name           string
		id             core.TrackedCommitID
		wantSuperseded *core.TrackedCommitID
	}{
		{name: "enqueued commit untouched", id: enqueued.ID},
		{name: "failed commit superseded by the next push", id: failed.ID, wantSuperseded: &pending.ID},
		{name: "pending commit superseded", id: pending.ID, wantSuperseded: &head.ID},
		{name: "other app untouched", id: other.ID},
		{name: "head stays open", id: head.ID},
	}
	for _, tt := range tests {
		got := mustGetTracked(t, st, tt.id)
		switch {
		case tt.wantSuperseded == nil && got.SupersededByID != nil:
			t.Errorf("%s: superseded by %d, want not superseded", tt.name, *got.SupersededByID)
		case tt.wantSuperseded != nil && (got.SupersededByID == nil || *got.SupersededByID != *tt.wantSuperseded):
			t.Errorf("%s: superseded by %v, want %d", tt.name, got.SupersededByID, *tt.wantSuperseded)
		}
	}
}

func TestTrackCommitIgnoresPushesThatAreNoLongerTheHead(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	newer := mustTrack(t, st, newTrackedCommit("blog", shaOf('b')))

	got, outcome, err := st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('a')), isHead(false))
	if err != nil {
		t.Fatalf("TrackCommit of a late delivery: %v", err)
	}
	if outcome != core.TrackStale || got.ID != 0 {
		t.Errorf("TrackCommit of a late delivery = %+v, %s, want %s and no commit", got, outcome, core.TrackStale)
	}
	if got := mustGetTracked(t, st, newer.ID); !got.Open() {
		t.Errorf("a late delivery of an older push closed the branch head: %+v", got)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM tracked_commits"); n != 1 {
		t.Errorf("tracked_commits has %d rows, want only the head", n)
	}

	enqueued := mustEnqueueCommit(t, st, newer)
	if _, outcome, err := st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('b')), isHead(false)); err != nil || outcome != core.TrackStale {
		t.Errorf("TrackCommit of a known commit that is no longer the head = %s, %v, want %s", outcome, err, core.TrackStale)
	}
	if got := mustGetTracked(t, st, enqueued.ID); got.EnqueuedAt == nil {
		t.Errorf("stale push changed the enqueued commit: %+v", got)
	}
}

func TestTrackCommitReopensSupersededCommitThatIsTheHeadAgain(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	first := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	second := mustTrack(t, st, newTrackedCommit("blog", shaOf('b')))

	reopened, outcome, err := st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('a')), isHead(true))
	if err != nil {
		t.Fatalf("TrackCommit after a force-push back: %v", err)
	}
	if outcome != core.TrackReopened || reopened.ID != first.ID || !reopened.Open() {
		t.Errorf("TrackCommit after a force-push back = %+v, %s, want the first row reopened", reopened, outcome)
	}
	if got := mustGetTracked(t, st, second.ID); got.SupersededByID == nil || *got.SupersededByID != first.ID {
		t.Errorf("commit force-pushed away = %+v, want superseded by %d", got, first.ID)
	}

	mustEnqueueCommit(t, st, reopened)
	third := mustTrack(t, st, newTrackedCommit("blog", shaOf('c')))
	again, outcome, err := st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('a')), isHead(true))
	if err != nil {
		t.Fatalf("TrackCommit of an enqueued commit that is the head again: %v", err)
	}
	if outcome != core.TrackExisting || again.ID != first.ID || again.SupersededByID != nil || again.EnqueuedAt == nil {
		t.Errorf("TrackCommit of an enqueued head = %+v, %s, want the enqueued row unchanged", again, outcome)
	}
	if got := mustGetTracked(t, st, third.ID); got.SupersededByID == nil || *got.SupersededByID != first.ID {
		t.Errorf("commit force-pushed away = %+v, want superseded by %d", got, first.ID)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployments"); n != 1 {
		t.Errorf("%d deployments, want 1", n)
	}
}

func TestTrackCommitHeadCheckError(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	head := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	boom := errors.New("github unavailable")
	_, _, err := st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('b')), func(context.Context) (bool, error) { return false, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("TrackCommit error = %v, want %v", err, boom)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM tracked_commits"); n != 1 {
		t.Errorf("tracked_commits has %d rows after a failed head check, want 1", n)
	}
	if got := mustGetTracked(t, st, head.ID); !got.Open() {
		t.Errorf("failed head check closed the previous head: %+v", got)
	}
}

func TestTrackCommitChecksTheHeadUnderTheAppLock(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	slowHead := func(ctx context.Context) (bool, error) {
		close(entered)
		<-release
		return true, ctx.Err()
	}

	var (
		wg      sync.WaitGroup
		errs    [2]error
		newerAt time.Time
	)
	wg.Go(func() {
		_, _, errs[0] = st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('a')), slowHead)
	})
	<-entered
	wg.Go(func() {
		_, _, errs[1] = st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('b')), isHead(true))
		newerAt = time.Now()
	})
	time.Sleep(200 * time.Millisecond)
	releasedAt := time.Now()
	close(release)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent TrackCommit: %v", err)
		}
	}
	if newerAt.Before(releasedAt) {
		t.Error("the newer push was tracked while the older push held the app lock")
	}
	commits, err := st.TrackedCommits(t.Context(), "acme/blog", shaOf('b'))
	if err != nil || len(commits) != 1 || !commits[0].Open() {
		t.Fatalf("newer push = %+v, %v, want it open", commits, err)
	}
	older, err := st.TrackedCommits(t.Context(), "acme/blog", shaOf('a'))
	if err != nil || len(older) != 1 || older[0].SupersededByID == nil || *older[0].SupersededByID != commits[0].ID {
		t.Errorf("older push = %+v, %v, want superseded by the newer push", older, err)
	}
}

func TestTrackCommitSameSHAForTwoApps(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	staging := newTrackedCommit("blog-staging", shaOf('a'))
	staging.Commit.Repository = "acme/blog"
	prod := newTrackedCommit("blog-prod", shaOf('a'))
	prod.Commit.Repository = "acme/blog"

	first := mustTrack(t, st, staging)
	second := mustTrack(t, st, prod)
	if first.ID == second.ID {
		t.Fatal("two apps share one tracked commit row")
	}
	mustSettle(t, st, first, evaluation(core.ChecksFailure, evaluatedAt))
	if got := mustGetTracked(t, st, second.ID); got.ChecksState != core.ChecksPending || !got.Open() {
		t.Errorf("settling one app changed the other: %+v", got)
	}

	commits, err := st.TrackedCommits(t.Context(), "ACME/Blog", shaOf('a'))
	if err != nil {
		t.Fatalf("TrackedCommits: %v", err)
	}
	if len(commits) != 2 || commits[0].ID != first.ID || commits[1].ID != second.ID {
		t.Errorf("TrackedCommits = %+v, want both apps in id order", commits)
	}
}

func TestTrackedCommitsLookup(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	tracked := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	mustTrack(t, st, newTrackedCommit("api", shaOf('a')))

	tests := []struct {
		name       string
		repository string
		sha        string
		want       []core.TrackedCommitID
	}{
		{name: "exact", repository: "Acme/blog", sha: shaOf('a'), want: []core.TrackedCommitID{tracked.ID}},
		{name: "case-insensitive repository", repository: "acme/BLOG", sha: shaOf('a'), want: []core.TrackedCommitID{tracked.ID}},
		{name: "other repository", repository: "acme/web", sha: shaOf('a')},
		{name: "untracked SHA", repository: "acme/blog", sha: shaOf('f')},
		{name: "invalid SHA", repository: "acme/blog", sha: "main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.TrackedCommits(t.Context(), tt.repository, tt.sha)
			if err != nil {
				t.Fatalf("TrackedCommits: %v", err)
			}
			if ids := trackedIDs(got); !slices.Equal(ids, tt.want) && len(ids)+len(tt.want) > 0 {
				t.Errorf("TrackedCommits = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestTrackedCommitNotFound(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if _, err := st.GetTrackedCommit(t.Context(), 42); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetTrackedCommit error = %v, want ErrNotFound", err)
	}
	if _, _, err := st.SettleTrackedCommit(t.Context(), 42, evaluation(core.ChecksSuccess, evaluatedAt), refuseEnqueue(t)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("SettleTrackedCommit error = %v, want ErrNotFound", err)
	}
}

func TestSettleTrackedCommitSavesEvidence(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	checks := []core.Check{
		{Name: "lint", Kind: core.CheckKindStatus, State: core.ChecksSuccess},
		{Name: "test", Kind: core.CheckKindRun, State: core.ChecksFailure, Detail: "failure"},
	}
	saved, promoted := mustSettle(t, st, c, evaluation(core.ChecksFailure, evaluatedAt, checks...))
	if promoted || saved.ChecksState != core.ChecksFailure || saved.EvaluatedAt == nil || !saved.EvaluatedAt.Equal(evaluatedAt) || !saved.Open() {
		t.Errorf("SettleTrackedCommit = %+v, %v", saved, promoted)
	}
	if !slices.Equal(saved.Checks, checks) {
		t.Errorf("saved checks = %+v, want %+v", saved.Checks, checks)
	}

	pending, promoted := mustSettle(t, st, c, evaluation(core.ChecksPending, evaluatedAt.Add(time.Second)))
	if promoted || pending.ChecksState != core.ChecksPending || len(pending.Checks) != 0 {
		t.Errorf("re-run = %+v, %v, want pending with no checks", pending, promoted)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM tracked_commits WHERE checks = '[]'::jsonb"); n != 1 {
		t.Errorf("nil checks stored as %d empty arrays, want 1", n)
	}
}

func TestSettleTrackedCommitRejectsInvalidEvaluations(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	tests := []struct {
		name string
		eval core.ChecksEvaluation
		want string
	}{
		{name: "unknown state", eval: evaluation("neutral", evaluatedAt), want: "checks state"},
		{name: "no read time", eval: evaluation(core.ChecksSuccess, time.Time{}), want: "time the checks were read"},
	}
	for _, tt := range tests {
		if _, _, err := st.SettleTrackedCommit(t.Context(), c.ID, tt.eval, refuseEnqueue(t)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: SettleTrackedCommit error = %v, want %q", tt.name, err, tt.want)
		}
	}
	if got := mustGetTracked(t, st, c.ID); got.EvaluatedAt != nil {
		t.Errorf("invalid evaluation was saved: %+v", got)
	}
}

func TestSettleTrackedCommitEnqueuesOnce(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	promoted := mustEnqueueCommit(t, st, c)
	if promoted.DeploymentID == nil || promoted.EnqueuedAt == nil || promoted.Open() || promoted.ChecksState != core.ChecksSuccess {
		t.Fatalf("enqueued commit = %+v, want success, a deployment and closed", promoted)
	}
	d, err := st.GetDeployment(t.Context(), *promoted.DeploymentID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if d.Commit != c.Commit || d.State != core.StateQueued {
		t.Errorf("deployment = %+v", d)
	}

	again, ok, err := st.SettleTrackedCommit(t.Context(), c.ID, evaluation(core.ChecksFailure, evaluatedAt.Add(time.Hour)), refuseEnqueue(t))
	if err != nil || ok {
		t.Errorf("settling an enqueued commit = %v, %v, want false", ok, err)
	}
	if again.DeploymentID == nil || *again.DeploymentID != d.ID || again.ChecksState != core.ChecksSuccess || !again.UpdatedAt.Equal(promoted.UpdatedAt) {
		t.Errorf("settling an enqueued commit changed it: %+v", again)
	}
}

func TestSettleTrackedCommitIgnoresClosedCommits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		close func(t *testing.T, st *store.Store, c core.TrackedCommit)
	}{
		{name: "enqueued", close: func(t *testing.T, st *store.Store, c core.TrackedCommit) {
			t.Helper()
			mustEnqueueCommit(t, st, c)
		}},
		{name: "superseded", close: func(t *testing.T, st *store.Store, c core.TrackedCommit) {
			t.Helper()
			mustTrack(t, st, newTrackedCommit(c.Target.App, shaOf('b')))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
			tt.close(t, st, c)
			before := mustGetTracked(t, st, c.ID)
			got, ok, err := st.SettleTrackedCommit(t.Context(), c.ID, evaluation(core.ChecksSuccess, evaluatedAt.Add(time.Hour)), refuseEnqueue(t))
			if err != nil || ok {
				t.Fatalf("SettleTrackedCommit = %v, %v, want false", ok, err)
			}
			if got.ChecksState != before.ChecksState || !got.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("SettleTrackedCommit on a closed commit changed it: %+v, was %+v", got, before)
			}
		})
	}
}

func TestSettleTrackedCommitIgnoresOlderSnapshots(t *testing.T) {
	t.Parallel()

	older, newer := evaluatedAt, evaluatedAt.Add(time.Second)
	tests := []struct {
		name         string
		first        core.ChecksEvaluation
		second       core.ChecksEvaluation
		wantState    core.ChecksState
		wantEnqueued bool
	}{
		{
			name:  "newer pending saved first, then older success",
			first: evaluation(core.ChecksPending, newer), second: evaluation(core.ChecksSuccess, older),
			wantState: core.ChecksPending,
		},
		{
			name:  "older pending saved first, then newer success",
			first: evaluation(core.ChecksPending, older), second: evaluation(core.ChecksSuccess, newer),
			wantState: core.ChecksSuccess, wantEnqueued: true,
		},
		{
			name:  "newer failure saved first, then older success",
			first: evaluation(core.ChecksFailure, newer), second: evaluation(core.ChecksSuccess, older),
			wantState: core.ChecksFailure,
		},
		{
			name:  "same read time",
			first: evaluation(core.ChecksPending, older), second: evaluation(core.ChecksSuccess, older),
			wantState: core.ChecksPending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
			mustSettle(t, st, c, tt.first)
			got, promoted := mustSettle(t, st, c, tt.second)
			if got.ChecksState != tt.wantState || promoted != tt.wantEnqueued {
				t.Errorf("after both evaluations = %s, enqueued %v, want %s, enqueued %v", got.ChecksState, promoted, tt.wantState, tt.wantEnqueued)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM deployments"); n != boolInt(tt.wantEnqueued) {
				t.Errorf("%d deployments, want %d", n, boolInt(tt.wantEnqueued))
			}
		})
	}
}

func TestSettleTrackedCommitEnqueuesSavedSuccess(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	setChecksState(t, st, c.ID, core.ChecksSuccess, evaluatedAt.Add(time.Minute))

	got, promoted := mustSettle(t, st, c, evaluation(core.ChecksPending, evaluatedAt))
	if !promoted || got.ChecksState != core.ChecksSuccess || got.DeploymentID == nil {
		t.Errorf("settling a saved success with an older snapshot = %+v, %v, want it enqueued", got, promoted)
	}
}

func TestSettleTrackedCommitRollsBackOnEnqueueError(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	boom := errors.New("queue unavailable")

	_, ok, err := st.SettleTrackedCommit(t.Context(), c.ID, evaluation(core.ChecksSuccess, evaluatedAt), func(tx *store.Store) (core.DeploymentID, error) {
		if _, err := enqueueDeployment(t, c)(tx); err != nil {
			return 0, err
		}
		return 0, boom
	})
	if !errors.Is(err, boom) || ok {
		t.Fatalf("SettleTrackedCommit = %v, %v, want %v", ok, err, boom)
	}
	got := mustGetTracked(t, st, c.ID)
	if got.DeploymentID != nil || got.EnqueuedAt != nil || got.ChecksState != core.ChecksPending || got.EvaluatedAt != nil {
		t.Errorf("tracked commit after failed enqueue = %+v, want it unchanged", got)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployments"); n != 0 {
		t.Errorf("%d deployments left after failed enqueue, want 0", n)
	}

	if _, ok := mustSettle(t, st, c, evaluation(core.ChecksSuccess, evaluatedAt)); !ok {
		t.Error("retry with the same snapshot did not enqueue")
	}
}

func TestSettleTrackedCommitConcurrently(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))

	const settlers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		promoted int
		errs     []error
	)
	start := make(chan struct{})
	for i := range settlers {
		wg.Go(func() {
			<-start
			state := core.ChecksSuccess
			if i%2 == 1 {
				state = core.ChecksPending
			}
			eval := evaluation(state, evaluatedAt.Add(time.Duration(i)*time.Second))
			_, ok, err := st.SettleTrackedCommit(t.Context(), c.ID, eval, enqueueDeployment(t, c))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				promoted++
			}
		})
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("SettleTrackedCommit: %v", err)
	}
	if promoted > 1 {
		t.Errorf("%d settles enqueued, want at most 1", promoted)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployments"); n != promoted {
		t.Errorf("%d deployments created, want %d", n, promoted)
	}
	got := mustGetTracked(t, st, c.ID)
	if got.Open() && got.ChecksState != core.ChecksPending {
		t.Errorf("commit left open as %s", got.ChecksState)
	}
	if final, ok := mustSettle(t, st, c, evaluation(core.ChecksSuccess, evaluatedAt.Add(time.Hour))); final.Open() || promoted+boolInt(ok) != 1 {
		t.Errorf("after a final green snapshot = %+v, want exactly one enqueue overall", final)
	}
}

func TestTrackAndSettleConcurrently(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	old := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))

	var (
		wg       sync.WaitGroup
		promoted bool
		errs     [2]error
	)
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		_, promoted, errs[0] = st.SettleTrackedCommit(t.Context(), old.ID, evaluation(core.ChecksSuccess, evaluatedAt), enqueueDeployment(t, old))
	})
	wg.Go(func() {
		<-start
		_, _, errs[1] = st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('b')), isHead(true))
	})
	close(start)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent track and settle: %v", err)
		}
	}
	got := mustGetTracked(t, st, old.ID)
	if promoted == (got.SupersededByID != nil) {
		t.Errorf("promoted = %v, superseded by %v: exactly one must win", promoted, got.SupersededByID)
	}
	if want := boolInt(promoted); queryInt(t, st, "SELECT count(*) FROM deployments") != want {
		t.Errorf("deployments count does not match promoted = %v", promoted)
	}
}

func followed(app, repository, branch string) core.FollowedBranch {
	return core.FollowedBranch{Target: core.Target{App: app, Environment: "prod"}, Repository: repository, Branch: branch}
}

func TestOpenTrackedCommits(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	blog := mustTrack(t, st, newTrackedCommit("blog", shaOf('b')))
	api := mustTrack(t, st, newTrackedCommit("api", shaOf('c')))
	mustSettle(t, st, api, evaluation(core.ChecksPending, evaluatedAt.Add(2*time.Minute)))
	failed := mustTrack(t, st, newTrackedCommit("web", shaOf('d')))
	mustSettle(t, st, failed, evaluation(core.ChecksFailure, evaluatedAt))
	mustEnqueueCommit(t, st, mustTrack(t, st, newTrackedCommit("docs", shaOf('e'))))
	green := mustTrack(t, st, newTrackedCommit("worker", shaOf('f')))
	setChecksState(t, st, green.ID, core.ChecksSuccess, evaluatedAt.Add(time.Minute))
	mustTrack(t, st, newTrackedCommit("removed", shaOf('1')))
	moved := newTrackedCommit("moved", shaOf('2'))
	mustTrack(t, st, moved)
	rebranched := newTrackedCommit("rebranched", shaOf('3'))
	mustTrack(t, st, rebranched)

	all := []core.FollowedBranch{
		followed("blog", "acme/BLOG", "main"),
		followed("api", "Acme/api", "main"),
		followed("web", "Acme/web", "main"),
		followed("docs", "Acme/docs", "main"),
		followed("worker", "Acme/worker", "main"),
		followed("moved", "Acme/moved-v2", "main"),
		followed("rebranched", "Acme/rebranched", "release"),
	}
	tests := []struct {
		name     string
		followed []core.FollowedBranch
		limit    int
		want     []core.TrackedCommitID
	}{
		{name: "least recently evaluated first", followed: all, limit: 10, want: []core.TrackedCommitID{blog.ID, failed.ID, green.ID, api.ID}},
		{name: "limit", followed: all, limit: 2, want: []core.TrackedCommitID{blog.ID, failed.ID}},
		{name: "one app", followed: all[1:2], limit: 10, want: []core.TrackedCommitID{api.ID}},
		{name: "nothing followed", limit: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.OpenTrackedCommits(t.Context(), tt.followed, tt.limit)
			if err != nil {
				t.Fatalf("OpenTrackedCommits: %v", err)
			}
			if ids := trackedIDs(got); !slices.Equal(ids, tt.want) && len(ids)+len(tt.want) > 0 {
				t.Errorf("OpenTrackedCommits = %v, want %v", ids, tt.want)
			}
		})
	}
	if _, err := st.OpenTrackedCommits(t.Context(), all, 0); err == nil {
		t.Error("OpenTrackedCommits(0) accepted a non-positive limit")
	}
}

func TestTrackedCommitStaysClosedWhenItsDeploymentIsDeleted(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	promoted := mustEnqueueCommit(t, st, c)
	mustExec(t, st, "DELETE FROM deployments WHERE id = $1", *promoted.DeploymentID)
	got := mustGetTracked(t, st, c.ID)
	if got.DeploymentID != nil {
		t.Errorf("deployment_id = %d after the deployment was deleted, want null", *got.DeploymentID)
	}
	if got.Open() || got.EnqueuedAt == nil {
		t.Errorf("commit reopened after its deployment was deleted: %+v", got)
	}
	open, err := st.OpenTrackedCommits(t.Context(), []core.FollowedBranch{followed("blog", "acme/blog", "main")}, 10)
	if err != nil || len(open) != 0 {
		t.Errorf("OpenTrackedCommits = %+v, %v, want none", open, err)
	}
	if _, ok, err := st.SettleTrackedCommit(t.Context(), c.ID, evaluation(core.ChecksSuccess, evaluatedAt.Add(time.Hour)), refuseEnqueue(t)); err != nil || ok {
		t.Errorf("SettleTrackedCommit = %v, %v, want no new deployment", ok, err)
	}
}
