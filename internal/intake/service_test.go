package intake_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

var (
	shaA = strings.Repeat("a", 40)
	shaB = strings.Repeat("b", 40)
)

func blogApp(env string, required ...string) intake.App {
	return intake.App{
		Target:          core.Target{App: "blog-" + env, Environment: env},
		Repository:      "Example-User/blog",
		Branch:          "main",
		RequiredChecks:  required,
		ImageRepository: "ghcr.io/example-user/blog",
	}
}

func green(names ...string) intake.Snapshot {
	var s intake.Snapshot
	for i, name := range names {
		s.CheckRuns = append(s.CheckRuns, done(int64(i+1), name, "success"))
	}
	return s
}

type harness struct {
	commits  *fakeCommits
	checks   *fakeChecks
	branches *fakeBranches
	svc      *intake.Service
}

func newHarness(t *testing.T, apps ...intake.App) *harness {
	t.Helper()
	h := &harness{commits: newFakeCommits(), checks: newFakeChecks(), branches: newFakeBranches()}
	h.svc = h.service(t, apps...)
	return h
}

func (h *harness) service(t *testing.T, apps ...intake.App) *intake.Service {
	t.Helper()
	svc, err := intake.New(intake.Options{Apps: apps, Commits: h.commits, Checks: h.checks, Branches: h.branches, Now: newTickingClock().Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func push(sha string) intake.Push {
	return intake.Push{Repository: "example-user/blog", Branch: "main", SHA: sha, Source: core.SourceWebhook, PushedBy: "octocat"}
}

func (h *harness) push(t *testing.T, p intake.Push) []intake.Result {
	t.Helper()
	h.branches.set(p.Repository, p.Branch, p.SHA)
	return h.deliver(t, p)
}

func (h *harness) deliver(t *testing.T, p intake.Push) []intake.Result {
	t.Helper()
	results, err := h.svc.HandlePush(t.Context(), p)
	if err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	return results
}

func (h *harness) reevaluate(t *testing.T, sha string) []intake.Result {
	t.Helper()
	results, err := h.svc.Reevaluate(t.Context(), "example-user/blog", sha)
	if err != nil {
		t.Fatalf("Reevaluate: %v", err)
	}
	return results
}

func (h *harness) poll(t *testing.T) []intake.Result {
	t.Helper()
	open, err := h.svc.Open(t.Context(), 10)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var results []intake.Result
	for _, c := range open {
		r, err := h.svc.Reevaluate(t.Context(), c.Commit.Repository, c.Commit.SHA)
		if err != nil {
			t.Fatalf("Reevaluate(%s): %v", c.Commit.SHA, err)
		}
		results = append(results, r...)
	}
	return results
}

func TestNewValidatesApps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		apps    []intake.App
		mutate  func(*intake.Options)
		wantErr string
	}{
		{name: "valid", apps: []intake.App{blogApp("staging"), blogApp("prod", "test")}},
		{name: "no apps", apps: nil},
		{name: "missing environment", apps: []intake.App{{Target: core.Target{App: "blog"}, Repository: "a/b", Branch: "main", ImageRepository: "ghcr.io/a/b"}}, wantErr: "target environment"},
		{name: "repository without owner", apps: []intake.App{{Target: core.Target{App: "blog", Environment: "prod"}, Repository: "blog", Branch: "main", ImageRepository: "ghcr.io/a/b"}}, wantErr: "owner/name"},
		{name: "repository with extra segment", apps: []intake.App{{Target: core.Target{App: "blog", Environment: "prod"}, Repository: "a/b/c", Branch: "main", ImageRepository: "ghcr.io/a/b"}}, wantErr: "owner/name"},
		{name: "empty owner", apps: []intake.App{{Target: core.Target{App: "blog", Environment: "prod"}, Repository: "/b", Branch: "main", ImageRepository: "ghcr.io/a/b"}}, wantErr: "owner/name"},
		{name: "missing branch", apps: []intake.App{{Target: core.Target{App: "blog", Environment: "prod"}, Repository: "a/b", ImageRepository: "ghcr.io/a/b"}}, wantErr: "branch"},
		{name: "missing image", apps: []intake.App{{Target: core.Target{App: "blog", Environment: "prod"}, Repository: "a/b", Branch: "main"}}, wantErr: "image repository"},
		{name: "blank required check", apps: []intake.App{blogApp("prod", "test", " ")}, wantErr: "required check 1"},
		{name: "duplicate target", apps: []intake.App{blogApp("prod"), blogApp("prod", "test")}, wantErr: "duplicate target blog-prod/prod"},
		{name: "missing commits", mutate: func(o *intake.Options) { o.Commits = nil }, wantErr: "commits store"},
		{name: "missing checks", mutate: func(o *intake.Options) { o.Checks = nil }, wantErr: "check source"},
		{name: "missing branches", mutate: func(o *intake.Options) { o.Branches = nil }, wantErr: "branch source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := intake.Options{Apps: tt.apps, Commits: newFakeCommits(), Checks: newFakeChecks(), Branches: newFakeBranches()}
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			_, err := intake.New(opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestHandlePushIgnoresUnmatchedPushes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		push intake.Push
	}{
		{name: "other repository", push: intake.Push{Repository: "example-user/api", Branch: "main", SHA: shaA, Source: core.SourceWebhook}},
		{name: "other branch", push: intake.Push{Repository: "example-user/blog", Branch: "feature", SHA: shaA, Source: core.SourceWebhook}},
		{name: "branch differs in case", push: intake.Push{Repository: "example-user/blog", Branch: "Main", SHA: shaA, Source: core.SourceWebhook}},
		{name: "full ref instead of branch", push: intake.Push{Repository: "example-user/blog", Branch: "refs/heads/main", SHA: shaA, Source: core.SourceWebhook}},
		{name: "branch deleted", push: intake.Push{Repository: "example-user/blog", Branch: "main", SHA: strings.Repeat("0", 40), Source: core.SourceWebhook}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, blogApp("prod"))
			if results := h.push(t, tt.push); results != nil {
				t.Errorf("HandlePush = %+v, want nil", results)
			}
			if h.commits.count() != 0 || h.checks.callCount() != 0 || h.branches.calls != 0 {
				t.Errorf("tracked %d commits and made %d check and %d branch calls, want none", h.commits.count(), h.checks.callCount(), h.branches.calls)
			}
		})
	}
}

func TestHandlePushRejectsInvalidPushes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		push   intake.Push
		wantIs error
		want   string
	}{
		{name: "short SHA", push: intake.Push{Repository: "example-user/blog", Branch: "main", SHA: "abc1234", Source: core.SourceWebhook}, wantIs: core.ErrInvalidCommitSHA},
		{name: "uppercase SHA", push: intake.Push{Repository: "example-user/blog", Branch: "main", SHA: strings.ToUpper(shaA), Source: core.SourceWebhook}, wantIs: core.ErrInvalidCommitSHA},
		{name: "unknown source", push: intake.Push{Repository: "example-user/blog", Branch: "main", SHA: shaA, Source: "cron"}, want: `source "cron"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, blogApp("prod"))
			results, err := h.svc.HandlePush(t.Context(), tt.push)
			if err == nil || results != nil {
				t.Fatalf("HandlePush = %+v, %v, want an error", results, err)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if h.commits.count() != 0 {
				t.Errorf("invalid push tracked %d commits", h.commits.count())
			}
		})
	}
}

func TestHandlePushEnqueuesGreenCommit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod"))
	h.checks.set("example-user/blog", shaA, green("test"))
	p := push(shaA)
	p.Repository = "EXAMPLE-USER/Blog"
	p.Source = core.SourcePoll

	results := h.push(t, p)
	if len(results) != 1 || !results[0].Enqueued {
		t.Fatalf("HandlePush = %+v, want one enqueued result", results)
	}
	c := results[0].Commit
	if c.Commit.Repository != "Example-User/blog" || c.Branch != "main" || c.ChecksState != core.ChecksSuccess || c.DeploymentID == nil || c.EnqueuedAt == nil {
		t.Errorf("tracked commit = %+v, want the configured repository, success and a deployment", c)
	}
	if len(c.Checks) != 1 || c.Checks[0].Name != "test" {
		t.Errorf("evidence = %+v", c.Checks)
	}

	deployments := h.commits.queue.all()
	if len(deployments) != 1 {
		t.Fatalf("enqueued %d deployments, want 1", len(deployments))
	}
	want := core.Deployment{
		ID:          *c.DeploymentID,
		Target:      core.Target{App: "blog-prod", Environment: "prod"},
		Kind:        core.KindDeploy,
		State:       core.StateQueued,
		Source:      core.SourcePoll,
		RequestedBy: "octocat",
		Commit:      core.Commit{Repository: "Example-User/blog", SHA: shaA},
		Image:       core.ImageRef{Repository: "ghcr.io/example-user/blog", Tag: shaA},
	}
	if deployments[0] != want {
		t.Errorf("deployment = %+v\nwant %+v", deployments[0], want)
	}
	if call := h.checks.lastCall(); call.repository != "Example-User/blog" || call.sha != shaA || call.names != nil {
		t.Errorf("Checks call = %+v, want every check of the commit", call)
	}
}

func TestHandlePushTwoAppsShareOneSnapshot(t *testing.T) {
	t.Parallel()

	staging := blogApp("staging", "test")
	prod := blogApp("prod", "test", "e2e")
	other := intake.App{Target: core.Target{App: "api", Environment: "prod"}, Repository: "example-user/api", Branch: "main", ImageRepository: "ghcr.io/example-user/api"}
	h := newHarness(t, staging, prod, other)
	h.checks.set("example-user/blog", shaA, green("test"))

	results := h.push(t, push(shaA))
	if len(results) != 2 {
		t.Fatalf("HandlePush returned %d results, want 2", len(results))
	}
	byTarget := map[core.Target]intake.Result{}
	for _, r := range results {
		byTarget[r.Commit.Target] = r
	}
	if r := byTarget[staging.Target]; !r.Enqueued || r.Commit.ChecksState != core.ChecksSuccess {
		t.Errorf("staging = %+v, want enqueued", r)
	}
	if r := byTarget[prod.Target]; r.Enqueued || r.Commit.ChecksState != core.ChecksPending {
		t.Errorf("prod = %+v, want pending on e2e", r)
	}
	if n := h.checks.callCount(); n != 1 {
		t.Errorf("made %d Checks calls, want 1", n)
	}
	if call := h.checks.lastCall(); !slices.Equal(call.names, []string{"test", "e2e"}) {
		t.Errorf("Checks call = %+v, want only the required checks of both apps", call)
	}

	h.checks.set("example-user/blog", shaA, green("test", "e2e"))
	results = h.reevaluate(t, shaA)
	if len(results) != 1 || results[0].Commit.Target != prod.Target || !results[0].Enqueued {
		t.Errorf("Reevaluate = %+v, want only prod enqueued", results)
	}
	if n := len(h.commits.queue.all()); n != 2 {
		t.Errorf("enqueued %d deployments, want 2", n)
	}

	h.checks.set("example-user/api", shaA, green("build"))
	results = h.push(t, intake.Push{Repository: "example-user/api", Branch: "main", SHA: shaA, Source: core.SourceWebhook})
	if len(results) != 1 || results[0].Commit.Target != other.Target || !results[0].Enqueued {
		t.Errorf("push to another repository with the same SHA = %+v, want only api enqueued", results)
	}
	if call := h.checks.lastCall(); call.repository != "example-user/api" || call.names != nil {
		t.Errorf("Checks call = %+v, want every check of example-user/api", call)
	}
}

func TestHandlePushReadsEveryCheckWhenAnyAppNeedsAllChecks(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod", "test"))
	h.push(t, push(shaA))
	if n := h.checks.callCount(); n != 1 {
		t.Fatalf("made %d Checks calls, want 1", n)
	}
	if names := h.checks.lastCall().names; names != nil {
		t.Errorf("Checks asked only for %v although one app needs every check", names)
	}
}

func TestHandlePushIgnoresDeliveriesThatAreNoLongerTheHead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		head string
	}{
		{name: "newer push already processed", head: shaB},
		{name: "branch deleted since", head: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, blogApp("prod", "test"))
			var newer core.TrackedCommit
			if tt.head != "" {
				newer = h.push(t, push(tt.head))[0].Commit
			}
			h.branches.set("example-user/blog", "main", tt.head)
			h.checks.set("example-user/blog", shaA, green("test"))
			calls := h.checks.callCount()

			if results := h.deliver(t, push(shaA)); len(results) != 0 {
				t.Errorf("late delivery of an older push = %+v, want nothing", results)
			}
			if h.checks.callCount() != calls || len(h.commits.queue.all()) != 0 {
				t.Error("late delivery of an older push was evaluated or enqueued")
			}
			if tracked, _ := h.commits.Tracked(t.Context(), "example-user/blog", shaA); len(tracked) != 0 {
				t.Errorf("late delivery of an older push was tracked: %+v", tracked)
			}
			if tt.head != "" {
				if got := h.commits.get(newer.ID); !got.Open() {
					t.Errorf("late delivery closed the branch head: %+v", got)
				}
			}
		})
	}
}

func TestHandlePushOutOfOrderDeliveryDeploysTheHead(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	h.branches.set("example-user/blog", "main", shaB)
	h.checks.set("example-user/blog", shaA, green("test"))
	h.checks.set("example-user/blog", shaB, green("test"))

	newer := h.deliver(t, push(shaB))
	older := h.deliver(t, push(shaA))
	if len(newer) != 1 || !newer[0].Enqueued || len(older) != 0 {
		t.Fatalf("deliveries B then A = %+v, %+v, want B enqueued and A ignored", newer, older)
	}
	deployments := h.commits.queue.all()
	if len(deployments) != 1 || deployments[0].Commit.SHA != shaB {
		t.Errorf("deployments = %+v, want only the head %s", deployments, shaB)
	}
}

func TestHandlePushForcePushBackReopensSupersededCommit(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	first := h.push(t, push(shaA))[0].Commit
	second := h.push(t, push(shaB))[0].Commit
	h.checks.set("example-user/blog", shaA, green("test"))

	results := h.push(t, push(shaA))
	if len(results) != 1 || results[0].Commit.ID != first.ID || !results[0].Enqueued {
		t.Fatalf("force-push back to %s = %+v, want the first commit reopened and enqueued", shaA, results)
	}
	if got := h.commits.get(second.ID); got.SupersededByID == nil || *got.SupersededByID != first.ID {
		t.Errorf("commit force-pushed away = %+v, want superseded by %d", got, first.ID)
	}
}

func TestHandlePushBranchHeadError(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod"))
	h.branches.fail(github.ErrNotInstalled)
	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, github.ErrNotInstalled) || !strings.Contains(err.Error(), "blog-staging/staging") || !strings.Contains(err.Error(), "blog-prod/prod") {
		t.Errorf("HandlePush error = %v, want ErrNotInstalled for both apps", err)
	}
	if len(results) != 0 || h.commits.count() != 0 || h.checks.callCount() != 0 {
		t.Errorf("HandlePush = %+v, tracked %d, want nothing tracked", results, h.commits.count())
	}
}

func TestOpenThenReevaluateEnqueues(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	results := h.push(t, push(shaA))
	if len(results) != 1 || results[0].Enqueued || results[0].Commit.ChecksState != core.ChecksPending {
		t.Fatalf("HandlePush = %+v, want pending", results)
	}
	open, err := h.svc.Open(t.Context(), 10)
	if err != nil || len(open) != 1 || open[0].ID != results[0].Commit.ID {
		t.Fatalf("Open = %+v, %v", open, err)
	}

	h.checks.set("example-user/blog", shaA, intake.Snapshot{CheckRuns: []github.CheckRun{run(1, "test", "in_progress", "")}})
	if results := h.reevaluate(t, shaA); len(results) != 1 || results[0].Enqueued {
		t.Fatalf("Reevaluate while running = %+v", results)
	}

	h.checks.set("example-user/blog", shaA, green("test"))
	results = h.reevaluate(t, shaA)
	if len(results) != 1 || !results[0].Enqueued {
		t.Fatalf("Reevaluate when green = %+v, want enqueued", results)
	}
	if open, _ := h.svc.Open(t.Context(), 10); len(open) != 0 {
		t.Errorf("Open after enqueue = %+v", open)
	}
}

func TestOpenAsksOnlyForFollowedBranches(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod", "test"))
	if _, err := h.svc.Open(t.Context(), 5); err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []core.FollowedBranch{
		{Target: blogApp("staging").Target, Repository: "Example-User/blog", Branch: "main"},
		{Target: blogApp("prod").Target, Repository: "Example-User/blog", Branch: "main"},
	}
	if len(h.commits.openCalls) != 1 || !slices.Equal(h.commits.openCalls[0], want) {
		t.Errorf("Open asked for %+v, want %+v", h.commits.openCalls, want)
	}

	empty := newHarness(t)
	if open, err := empty.svc.Open(t.Context(), 5); err != nil || open != nil || len(empty.commits.openCalls) != 0 {
		t.Errorf("Open without apps = %+v, %v, %d store calls, want nothing", open, err, len(empty.commits.openCalls))
	}
}

func TestOpenSkipsCommitsOfAppsNoLongerFollowingTheirBranch(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod"), blogApp("removed"))
	h.push(t, push(shaA))
	moved := blogApp("prod")
	moved.Branch = "release"
	current := h.service(t, moved)
	if open, err := current.Open(t.Context(), 10); err != nil || len(open) != 0 {
		t.Errorf("Open = %+v, %v, want no commits from the old branch or removed apps", open, err)
	}
}

func TestFailureThenRerunEnqueues(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	h.checks.set("example-user/blog", shaA, intake.Snapshot{CheckRuns: []github.CheckRun{done(1, "test", "failure")}})
	results := h.push(t, push(shaA))
	if len(results) != 1 || results[0].Commit.ChecksState != core.ChecksFailure || results[0].Enqueued {
		t.Fatalf("HandlePush = %+v, want failure", results)
	}

	h.checks.set("example-user/blog", shaA, intake.Snapshot{CheckRuns: []github.CheckRun{done(1, "test", "failure"), done(2, "test", "success")}})
	results = h.poll(t)
	if len(results) != 1 || !results[0].Enqueued || results[0].Commit.ChecksState != core.ChecksSuccess {
		t.Errorf("poll after re-run = %+v, want enqueued", results)
	}
}

func TestSupersededCommitIsNeverEnqueued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	h.push(t, push(shaA))
	h.push(t, push(shaB))
	calls := h.checks.callCount()

	h.checks.set("example-user/blog", shaA, green("test"))
	if results := h.reevaluate(t, shaA); len(results) != 0 {
		t.Errorf("Reevaluate of a superseded commit = %+v, want none", results)
	}
	if results := h.deliver(t, push(shaA)); len(results) != 0 {
		t.Errorf("redelivered push of a superseded SHA = %+v, want none", results)
	}
	if h.checks.callCount() != calls {
		t.Error("a superseded commit was evaluated")
	}
	if n := len(h.commits.queue.all()); n != 0 {
		t.Errorf("enqueued %d deployments, want 0", n)
	}
}

func TestEnqueuedCommitIsNotEnqueuedAgain(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	h.checks.set("example-user/blog", shaA, green("test"))
	first := h.push(t, push(shaA))
	if len(first) != 1 || !first[0].Enqueued {
		t.Fatalf("HandlePush = %+v, want enqueued", first)
	}
	calls := h.checks.callCount()

	again := h.push(t, push(shaA))
	if len(again) != 1 || again[0].Enqueued || again[0].Commit.DeploymentID == nil || *again[0].Commit.DeploymentID != *first[0].Commit.DeploymentID {
		t.Errorf("redelivered push = %+v, want the enqueued commit unchanged", again)
	}
	if results := h.reevaluate(t, shaA); len(results) != 0 {
		t.Errorf("Reevaluate of an enqueued commit = %+v, want none", results)
	}
	if results := h.poll(t); len(results) != 0 {
		t.Errorf("poll after enqueue = %+v, want none", results)
	}
	if h.checks.callCount() != calls {
		t.Error("an enqueued commit was evaluated again")
	}
	if n := len(h.commits.queue.all()); n != 1 {
		t.Errorf("enqueued %d deployments, want 1", n)
	}
}

func TestReevaluateSkipsUntrackedAndUnconfiguredCommits(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod"), blogApp("staging"))
	h.push(t, push(shaA))
	movedRepository := blogApp("prod")
	movedRepository.Repository = "example-user/blog-v2"
	movedBranch := blogApp("staging")
	movedBranch.Branch = "release"
	current := h.service(t, movedRepository, movedBranch, blogApp("other"))
	calls := h.checks.callCount()

	tests := []struct {
		name       string
		repository string
		sha        string
	}{
		{name: "untracked SHA", repository: "example-user/blog", sha: shaB},
		{name: "untracked repository", repository: "example-user/other", sha: shaA},
		{name: "apps moved to another repository or branch", repository: "example-user/blog", sha: shaA},
	}
	for _, tt := range tests {
		results, err := current.Reevaluate(t.Context(), tt.repository, tt.sha)
		if err != nil || results != nil {
			t.Errorf("%s: Reevaluate = %+v, %v, want nothing", tt.name, results, err)
		}
	}
	if h.checks.callCount() != calls {
		t.Errorf("Reevaluate of untracked commits made %d Checks calls", h.checks.callCount()-calls)
	}
	if _, err := current.Reevaluate(t.Context(), "example-user/blog", "main"); !errors.Is(err, core.ErrInvalidCommitSHA) {
		t.Errorf("Reevaluate with a branch name error = %v, want ErrInvalidCommitSHA", err)
	}
}

func TestCheckSourceErrorLeavesCommitPending(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod"))
	h.checks.fail(github.ErrNotInstalled)
	h.branches.set("example-user/blog", "main", shaA)
	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, github.ErrNotInstalled) {
		t.Fatalf("HandlePush error = %v, want ErrNotInstalled", err)
	}
	if len(results) != 2 {
		t.Fatalf("HandlePush returned %d results, want both tracked commits", len(results))
	}
	for _, r := range results {
		if r.Enqueued || r.Commit.ChecksState != core.ChecksPending || r.Commit.ID == 0 {
			t.Errorf("result = %+v, want tracked and pending", r)
		}
	}
	if n := h.checks.callCount(); n != 1 {
		t.Errorf("made %d Checks calls, want 1 shared failing call", n)
	}

	h.checks.fail(nil)
	h.checks.set("example-user/blog", shaA, green("test"))
	if results := h.reevaluate(t, shaA); len(results) != 2 || !results[0].Enqueued || !results[1].Enqueued {
		t.Errorf("Reevaluate after GitHub recovered = %+v, want both enqueued", results)
	}
}

func TestEnqueueErrorIsRetriedByThePoller(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod"))
	h.checks.set("example-user/blog", shaA, green("test"))
	h.commits.queue.fail(errBoom)
	h.branches.set("example-user/blog", "main", shaA)

	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, errBoom) {
		t.Fatalf("HandlePush error = %v, want %v", err, errBoom)
	}
	if len(results) != 1 || results[0].Enqueued || results[0].Commit.DeploymentID != nil {
		t.Fatalf("result = %+v, want not enqueued", results)
	}
	if got := h.commits.get(results[0].Commit.ID); !got.Open() {
		t.Errorf("commit closed after failed enqueue: %+v", got)
	}

	h.commits.queue.fail(nil)
	if results := h.poll(t); len(results) != 1 || !results[0].Enqueued {
		t.Errorf("poll after queue recovered = %+v, want enqueued", results)
	}
}

func TestErrorsForOneAppDoNotBlockAnother(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod"))
	h.commits.trackErr[blogApp("staging").Target] = errBoom
	h.checks.set("example-user/blog", shaA, green("test"))
	h.branches.set("example-user/blog", "main", shaA)

	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "blog-staging/staging") {
		t.Errorf("HandlePush error = %v, want the staging failure", err)
	}
	if len(results) != 1 || results[0].Commit.Target != blogApp("prod").Target || !results[0].Enqueued {
		t.Errorf("HandlePush = %+v, want prod enqueued", results)
	}
}

func TestSettleErrorIsReturned(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod"))
	h.checks.set("example-user/blog", shaA, green("test"))
	h.commits.settleErr = errBoom
	h.branches.set("example-user/blog", "main", shaA)
	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, errBoom) {
		t.Errorf("HandlePush error = %v, want %v", err, errBoom)
	}
	if len(results) != 1 || results[0].Enqueued {
		t.Errorf("HandlePush = %+v, want one unenqueued result", results)
	}
	if n := len(h.commits.queue.all()); n != 0 {
		t.Errorf("enqueued %d deployments after failing to settle", n)
	}
}

func TestOlderSnapshotNeverOverridesNewerOne(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		older        intake.Snapshot
		newer        intake.Snapshot
		wantState    core.ChecksState
		wantEnqueued bool
	}{
		{
			name:      "stale green read after a re-run started",
			older:     green("test"),
			newer:     intake.Snapshot{CheckRuns: []github.CheckRun{run(2, "test", "in_progress", "")}},
			wantState: core.ChecksPending,
		},
		{
			name:         "stale pending read after checks turned green",
			older:        intake.Snapshot{CheckRuns: []github.CheckRun{run(1, "test", "in_progress", "")}},
			newer:        green("test"),
			wantState:    core.ChecksSuccess,
			wantEnqueued: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, blogApp("prod", "test"))
			h.checks.set("example-user/blog", shaA, intake.Snapshot{CheckRuns: []github.CheckRun{run(1, "test", "queued", "")}})
			tracked := h.push(t, push(shaA))[0].Commit

			olderRead := make(chan struct{})
			newerSettled := make(chan struct{})
			h.checks.onRead = func(call int, s intake.Snapshot) intake.Snapshot {
				switch call {
				case 2:
					close(olderRead)
					<-newerSettled
					return tt.older
				case 3:
					return tt.newer
				}
				return s
			}

			var (
				wg    sync.WaitGroup
				older []intake.Result
				err   error
			)
			wg.Go(func() { older, err = h.svc.Reevaluate(t.Context(), "example-user/blog", shaA) })
			<-olderRead
			newer := h.reevaluate(t, shaA)
			close(newerSettled)
			wg.Wait()
			if err != nil {
				t.Fatalf("older Reevaluate: %v", err)
			}

			got := h.commits.get(tracked.ID)
			if got.ChecksState != tt.wantState {
				t.Errorf("saved state = %s, want %s from the newer snapshot", got.ChecksState, tt.wantState)
			}
			enqueued := len(h.commits.queue.all())
			if enqueued != boolInt(tt.wantEnqueued) {
				t.Errorf("enqueued %d deployments, want %d", enqueued, boolInt(tt.wantEnqueued))
			}
			if len(newer) != 1 || newer[0].Enqueued != tt.wantEnqueued || len(older) != 1 || older[0].Enqueued {
				t.Errorf("results = newer %+v, older %+v", newer, older)
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
