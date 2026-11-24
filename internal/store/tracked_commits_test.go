package store_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

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

func mustTrack(t *testing.T, st *store.Store, c core.TrackedCommit) core.TrackedCommit {
	t.Helper()
	tracked, _, err := st.TrackCommit(t.Context(), c)
	if err != nil {
		t.Fatalf("TrackCommit(%s): %v", c.Commit.SHA, err)
	}
	return tracked
}

func mustSaveChecks(t *testing.T, st *store.Store, id core.TrackedCommitID, state core.ChecksState, checks ...core.Check) core.TrackedCommit {
	t.Helper()
	c, err := st.SaveCommitChecks(t.Context(), id, state, checks)
	if err != nil {
		t.Fatalf("SaveCommitChecks(%d): %v", id, err)
	}
	return c
}

func mustGetTracked(t *testing.T, st *store.Store, id core.TrackedCommitID) core.TrackedCommit {
	t.Helper()
	c, err := st.GetTrackedCommit(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTrackedCommit(%d): %v", id, err)
	}
	return c
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

func TestTrackCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	in := newTrackedCommit("blog", shaOf('a'))
	got, created, err := st.TrackCommit(t.Context(), in)
	if err != nil {
		t.Fatalf("TrackCommit: %v", err)
	}
	if !created {
		t.Error("created = false for a new commit")
	}
	if got.ID == 0 || got.Target != in.Target || got.Commit != in.Commit || got.Branch != "main" ||
		got.Source != core.SourceWebhook || got.PushedBy != "octocat" || got.ChecksState != core.ChecksPending {
		t.Errorf("TrackCommit = %+v", got)
	}
	if len(got.Checks) != 0 || got.EvaluatedAt != nil || !got.Open() || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("new tracked commit is not empty and open: %+v", got)
	}

	again := in
	again.Source = core.SourcePoll
	again.PushedBy = "someone-else"
	repeat, created, err := st.TrackCommit(t.Context(), again)
	if err != nil {
		t.Fatalf("second TrackCommit: %v", err)
	}
	if created {
		t.Error("created = true for a known commit")
	}
	if repeat.ID != got.ID || repeat.Source != core.SourceWebhook || repeat.PushedBy != "octocat" {
		t.Errorf("second TrackCommit = %+v, want the original row %+v", repeat, got)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM tracked_commits"); n != 1 {
		t.Errorf("tracked_commits has %d rows, want 1", n)
	}
}

func TestTrackCommitRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	deployment := core.DeploymentID(1)
	newer := core.TrackedCommitID(1)
	tests := []struct {
		name    string
		mutate  func(*core.TrackedCommit)
		wantErr string
		wantIs  error
	}{
		{name: "short SHA", mutate: func(c *core.TrackedCommit) { c.Commit.SHA = "abc" }, wantIs: core.ErrInvalidCommitSHA},
		{name: "missing branch", mutate: func(c *core.TrackedCommit) { c.Branch = "" }, wantErr: "branch"},
		{name: "unknown source", mutate: func(c *core.TrackedCommit) { c.Source = "cron" }, wantErr: "source"},
		{name: "already evaluated", mutate: func(c *core.TrackedCommit) { c.ChecksState = core.ChecksSuccess }, wantErr: "start pending"},
		{name: "already enqueued", mutate: func(c *core.TrackedCommit) { c.DeploymentID = &deployment }, wantErr: "already be enqueued"},
		{name: "already superseded", mutate: func(c *core.TrackedCommit) { c.SupersededByID = &newer }, wantErr: "superseded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			c := newTrackedCommit("blog", shaOf('a'))
			tt.mutate(&c)
			_, _, err := st.TrackCommit(t.Context(), c)
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

func TestTrackCommitSupersedesOlderOpenCommits(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	enqueued := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	mustSaveChecks(t, st, enqueued.ID, core.ChecksSuccess)
	if _, ok, err := st.PromoteTrackedCommit(t.Context(), enqueued.ID, enqueueDeployment(t, enqueued)); err != nil || !ok {
		t.Fatalf("PromoteTrackedCommit = %v, %v", ok, err)
	}
	failed := mustTrack(t, st, newTrackedCommit("blog", shaOf('b')))
	mustSaveChecks(t, st, failed.ID, core.ChecksFailure)
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

	again := mustTrack(t, st, newTrackedCommit("blog", shaOf('c')))
	if again.ID != pending.ID || again.SupersededByID == nil || *again.SupersededByID != head.ID {
		t.Errorf("re-tracking a superseded SHA = %+v, want the superseded row", again)
	}
	if got := mustGetTracked(t, st, head.ID); !got.Open() {
		t.Errorf("re-tracking an older SHA superseded the head: %+v", got)
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
	mustSaveChecks(t, st, first.ID, core.ChecksFailure)
	if got := mustGetTracked(t, st, second.ID); got.ChecksState != core.ChecksPending {
		t.Errorf("saving checks for one app changed the other: %+v", got)
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
			if len(got) != len(tt.want) {
				t.Fatalf("TrackedCommits = %+v, want ids %v", got, tt.want)
			}
			for i, c := range got {
				if c.ID != tt.want[i] {
					t.Errorf("TrackedCommits[%d].ID = %d, want %d", i, c.ID, tt.want[i])
				}
			}
		})
	}
}

func TestGetTrackedCommitNotFound(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if _, err := st.GetTrackedCommit(t.Context(), 42); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetTrackedCommit error = %v, want ErrNotFound", err)
	}
	if _, err := st.SaveCommitChecks(t.Context(), 42, core.ChecksSuccess, nil); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("SaveCommitChecks error = %v, want ErrNotFound", err)
	}
	if _, _, err := st.PromoteTrackedCommit(t.Context(), 42, func(*store.Store) (core.DeploymentID, error) {
		t.Error("enqueue called for a missing commit")
		return 0, nil
	}); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("PromoteTrackedCommit error = %v, want ErrNotFound", err)
	}
}

func TestSaveCommitChecks(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	checks := []core.Check{
		{Name: "lint", Kind: core.CheckKindStatus, State: core.ChecksSuccess},
		{Name: "test", Kind: core.CheckKindRun, State: core.ChecksFailure, Detail: "failure"},
	}
	saved := mustSaveChecks(t, st, c.ID, core.ChecksFailure, checks...)
	if saved.ChecksState != core.ChecksFailure || saved.EvaluatedAt == nil {
		t.Errorf("SaveCommitChecks = %+v", saved)
	}
	if len(saved.Checks) != 2 || saved.Checks[0] != checks[0] || saved.Checks[1] != checks[1] {
		t.Errorf("saved checks = %+v, want %+v", saved.Checks, checks)
	}

	rerun := mustSaveChecks(t, st, c.ID, core.ChecksSuccess)
	if rerun.ChecksState != core.ChecksSuccess || len(rerun.Checks) != 0 {
		t.Errorf("re-evaluation after failure = %+v, want success with no checks", rerun)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM tracked_commits WHERE checks = '[]'::jsonb"); n != 1 {
		t.Errorf("nil checks stored as %d empty arrays, want 1", n)
	}

	if _, err := st.SaveCommitChecks(t.Context(), c.ID, "neutral", nil); err == nil {
		t.Error("SaveCommitChecks accepted an unknown state")
	}
}

func TestSaveCommitChecksIgnoresClosedCommits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		close func(t *testing.T, st *store.Store, c core.TrackedCommit)
	}{
		{name: "enqueued", close: func(t *testing.T, st *store.Store, c core.TrackedCommit) {
			t.Helper()
			mustSaveChecks(t, st, c.ID, core.ChecksSuccess)
			if _, ok, err := st.PromoteTrackedCommit(t.Context(), c.ID, enqueueDeployment(t, c)); err != nil || !ok {
				t.Fatalf("PromoteTrackedCommit = %v, %v", ok, err)
			}
		}},
		{name: "superseded", close: func(t *testing.T, st *store.Store, c core.TrackedCommit) {
			t.Helper()
			mustSaveChecks(t, st, c.ID, core.ChecksSuccess)
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
			got := mustSaveChecks(t, st, c.ID, core.ChecksFailure, core.Check{Name: "test", Kind: core.CheckKindRun, State: core.ChecksFailure})
			if got.ChecksState != core.ChecksSuccess || len(got.Checks) != 0 || !got.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("SaveCommitChecks on a closed commit changed it: %+v", got)
			}
		})
	}
}

func TestPromoteTrackedCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	mustSaveChecks(t, st, c.ID, core.ChecksSuccess)

	promoted, ok, err := st.PromoteTrackedCommit(t.Context(), c.ID, enqueueDeployment(t, c))
	if err != nil || !ok {
		t.Fatalf("PromoteTrackedCommit = %v, %v", ok, err)
	}
	if promoted.DeploymentID == nil || promoted.Open() {
		t.Fatalf("promoted commit = %+v, want a deployment id", promoted)
	}
	d, err := st.GetDeployment(t.Context(), *promoted.DeploymentID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if d.Commit != c.Commit || d.State != core.StateQueued {
		t.Errorf("deployment = %+v", d)
	}

	again, ok, err := st.PromoteTrackedCommit(t.Context(), c.ID, func(*store.Store) (core.DeploymentID, error) {
		t.Error("enqueue called for an enqueued commit")
		return 0, nil
	})
	if err != nil || ok {
		t.Errorf("second PromoteTrackedCommit = %v, %v, want false", ok, err)
	}
	if again.DeploymentID == nil || *again.DeploymentID != d.ID {
		t.Errorf("second PromoteTrackedCommit = %+v, want the existing deployment", again)
	}
}

func TestPromoteTrackedCommitRefusesIneligibleCommits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(t *testing.T, st *store.Store, c core.TrackedCommit)
	}{
		{name: "pending", prepare: func(*testing.T, *store.Store, core.TrackedCommit) {}},
		{name: "failure", prepare: func(t *testing.T, st *store.Store, c core.TrackedCommit) {
			t.Helper()
			mustSaveChecks(t, st, c.ID, core.ChecksFailure)
		}},
		{name: "superseded", prepare: func(t *testing.T, st *store.Store, c core.TrackedCommit) {
			t.Helper()
			mustSaveChecks(t, st, c.ID, core.ChecksSuccess)
			mustTrack(t, st, newTrackedCommit(c.Target.App, shaOf('b')))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
			tt.prepare(t, st, c)
			got, ok, err := st.PromoteTrackedCommit(t.Context(), c.ID, func(*store.Store) (core.DeploymentID, error) {
				t.Error("enqueue called for an ineligible commit")
				return 0, nil
			})
			if err != nil || ok {
				t.Errorf("PromoteTrackedCommit = %v, %v, want false", ok, err)
			}
			if got.ID != c.ID || got.DeploymentID != nil {
				t.Errorf("PromoteTrackedCommit returned %+v", got)
			}
		})
	}
}

func TestPromoteTrackedCommitRollsBackOnEnqueueError(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	mustSaveChecks(t, st, c.ID, core.ChecksSuccess)
	boom := errors.New("queue unavailable")

	_, ok, err := st.PromoteTrackedCommit(t.Context(), c.ID, func(tx *store.Store) (core.DeploymentID, error) {
		if _, err := enqueueDeployment(t, c)(tx); err != nil {
			return 0, err
		}
		return 0, boom
	})
	if !errors.Is(err, boom) || ok {
		t.Fatalf("PromoteTrackedCommit = %v, %v, want %v", ok, err, boom)
	}
	if got := mustGetTracked(t, st, c.ID); got.DeploymentID != nil || got.ChecksState != core.ChecksSuccess {
		t.Errorf("tracked commit after failed enqueue = %+v", got)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployments"); n != 0 {
		t.Errorf("%d deployments left after failed enqueue, want 0", n)
	}

	if _, ok, err := st.PromoteTrackedCommit(t.Context(), c.ID, enqueueDeployment(t, c)); err != nil || !ok {
		t.Errorf("retry PromoteTrackedCommit = %v, %v", ok, err)
	}
}

func TestPromoteTrackedCommitConcurrently(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	mustSaveChecks(t, st, c.ID, core.ChecksSuccess)

	const promoters = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		promoted int
		errs     []error
	)
	start := make(chan struct{})
	for range promoters {
		wg.Go(func() {
			<-start
			_, ok, err := st.PromoteTrackedCommit(t.Context(), c.ID, enqueueDeployment(t, c))
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
		t.Errorf("PromoteTrackedCommit: %v", err)
	}
	if promoted != 1 {
		t.Errorf("%d promotions succeeded, want 1", promoted)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM deployments"); n != 1 {
		t.Errorf("%d deployments created, want 1", n)
	}
}

func TestTrackAndPromoteConcurrently(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	old := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	mustSaveChecks(t, st, old.ID, core.ChecksSuccess)

	var (
		wg       sync.WaitGroup
		promoted bool
		errs     [2]error
	)
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		_, promoted, errs[0] = st.PromoteTrackedCommit(t.Context(), old.ID, enqueueDeployment(t, old))
	})
	wg.Go(func() {
		<-start
		_, _, errs[1] = st.TrackCommit(t.Context(), newTrackedCommit("blog", shaOf('b')))
	})
	close(start)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent track and promote: %v", err)
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

func TestPendingTrackedCommits(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	blog := mustTrack(t, st, newTrackedCommit("blog", shaOf('b')))
	api := mustTrack(t, st, newTrackedCommit("api", shaOf('c')))
	failed := mustTrack(t, st, newTrackedCommit("web", shaOf('d')))
	mustSaveChecks(t, st, failed.ID, core.ChecksFailure)
	enqueued := mustTrack(t, st, newTrackedCommit("docs", shaOf('e')))
	mustSaveChecks(t, st, enqueued.ID, core.ChecksSuccess)
	if _, ok, err := st.PromoteTrackedCommit(t.Context(), enqueued.ID, enqueueDeployment(t, enqueued)); err != nil || !ok {
		t.Fatalf("PromoteTrackedCommit = %v, %v", ok, err)
	}
	worker := mustTrack(t, st, newTrackedCommit("worker", shaOf('f')))

	tests := []struct {
		limit int
		want  []core.TrackedCommitID
	}{
		{limit: 10, want: []core.TrackedCommitID{blog.ID, api.ID, worker.ID}},
		{limit: 2, want: []core.TrackedCommitID{blog.ID, api.ID}},
	}
	for _, tt := range tests {
		got, err := st.PendingTrackedCommits(t.Context(), tt.limit)
		if err != nil {
			t.Fatalf("PendingTrackedCommits(%d): %v", tt.limit, err)
		}
		var ids []core.TrackedCommitID
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		if len(ids) != len(tt.want) {
			t.Fatalf("PendingTrackedCommits(%d) = %v, want %v", tt.limit, ids, tt.want)
		}
		for i := range ids {
			if ids[i] != tt.want[i] {
				t.Errorf("PendingTrackedCommits(%d) = %v, want %v", tt.limit, ids, tt.want)
			}
		}
	}
	if _, err := st.PendingTrackedCommits(t.Context(), 0); err == nil {
		t.Error("PendingTrackedCommits(0) accepted a non-positive limit")
	}
}

func TestTrackedCommitDeploymentDeleteSetsNull(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	c := mustTrack(t, st, newTrackedCommit("blog", shaOf('a')))
	mustSaveChecks(t, st, c.ID, core.ChecksSuccess)
	promoted, _, err := st.PromoteTrackedCommit(t.Context(), c.ID, enqueueDeployment(t, c))
	if err != nil {
		t.Fatalf("PromoteTrackedCommit: %v", err)
	}
	mustExec(t, st, "DELETE FROM deployments WHERE id = $1", *promoted.DeploymentID)
	if got := mustGetTracked(t, st, c.ID); got.DeploymentID != nil {
		t.Errorf("deployment_id = %d after the deployment was deleted, want null", *got.DeploymentID)
	}
}
