package intake_test

import (
	"errors"
	"strings"
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
	commits *fakeCommits
	checks  *fakeChecks
	svc     *intake.Service
}

func newHarness(t *testing.T, apps ...intake.App) *harness {
	t.Helper()
	h := &harness{commits: newFakeCommits(), checks: newFakeChecks()}
	svc, err := intake.New(intake.Options{Apps: apps, Commits: h.commits, Checks: h.checks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h
}

func push(sha string) intake.Push {
	return intake.Push{Repository: "example-user/blog", Branch: "main", SHA: sha, Source: core.SourceWebhook, PushedBy: "octocat"}
}

func (h *harness) push(t *testing.T, p intake.Push) []intake.Result {
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := intake.Options{Apps: tt.apps, Commits: newFakeCommits(), Checks: newFakeChecks()}
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
			if h.commits.count() != 0 || h.checks.callCount() != 0 {
				t.Errorf("tracked %d commits and made %d check calls, want none", h.commits.count(), h.checks.callCount())
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
	if c.Commit.Repository != "Example-User/blog" || c.Branch != "main" || c.ChecksState != core.ChecksSuccess || c.DeploymentID == nil {
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
	if call := h.checks.lastCall(); call != (checksCall{repository: "Example-User/blog", sha: shaA, withSuites: true}) {
		t.Errorf("Checks call = %+v", call)
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
	if call := h.checks.lastCall(); call.withSuites {
		t.Errorf("suites requested although every app lists required checks: %+v", call)
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
	if call := h.checks.lastCall(); call.repository != "example-user/api" || !call.withSuites {
		t.Errorf("Checks call = %+v, want example-user/api with suites", call)
	}
}

func TestHandlePushRequestsSuitesWhenAnyAppNeedsAllChecks(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod", "test"))
	h.push(t, push(shaA))
	if n := h.checks.callCount(); n != 1 {
		t.Fatalf("made %d Checks calls, want 1", n)
	}
	if !h.checks.lastCall().withSuites {
		t.Error("suites not requested for an app without required checks")
	}
}

func TestPendingThenReevaluateEnqueues(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	results := h.push(t, push(shaA))
	if len(results) != 1 || results[0].Enqueued || results[0].Commit.ChecksState != core.ChecksPending {
		t.Fatalf("HandlePush = %+v, want pending", results)
	}
	pending, err := h.svc.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 || pending[0].ID != results[0].Commit.ID {
		t.Fatalf("Pending = %+v, %v", pending, err)
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
	if pending, _ := h.svc.Pending(t.Context(), 10); len(pending) != 0 {
		t.Errorf("Pending after enqueue = %+v", pending)
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
	results = h.reevaluate(t, shaA)
	if len(results) != 1 || !results[0].Enqueued || results[0].Commit.ChecksState != core.ChecksSuccess {
		t.Errorf("Reevaluate after re-run = %+v, want enqueued", results)
	}
}

func TestSupersededCommitIsNeverEnqueued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod", "test"))
	older := h.push(t, push(shaA))[0].Commit
	h.push(t, push(shaB))
	calls := h.checks.callCount()

	h.checks.set("example-user/blog", shaA, green("test"))
	if results := h.reevaluate(t, shaA); len(results) != 0 {
		t.Errorf("Reevaluate of a superseded commit = %+v, want none", results)
	}
	if h.checks.callCount() != calls {
		t.Error("Reevaluate of a superseded commit called GitHub")
	}

	results := h.push(t, push(shaA))
	if len(results) != 1 || results[0].Enqueued || results[0].Commit.ID != older.ID || results[0].Commit.SupersededByID == nil {
		t.Errorf("re-pushing a superseded SHA = %+v, want the superseded commit, not enqueued", results)
	}
	if h.checks.callCount() != calls {
		t.Error("re-pushing a superseded SHA called GitHub")
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
	if h.checks.callCount() != calls {
		t.Error("an enqueued commit was evaluated again")
	}
	if n := len(h.commits.queue.all()); n != 1 {
		t.Errorf("enqueued %d deployments, want 1", n)
	}
}

func TestReevaluateSkipsUntrackedAndUnconfiguredCommits(t *testing.T) {
	t.Parallel()

	commits := newFakeCommits()
	checks := newFakeChecks()
	old, err := intake.New(intake.Options{Apps: []intake.App{blogApp("prod")}, Commits: commits, Checks: checks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := old.HandlePush(t.Context(), push(shaA)); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}
	moved := blogApp("prod")
	moved.Repository = "example-user/blog-v2"
	current, err := intake.New(intake.Options{Apps: []intake.App{blogApp("staging"), moved}, Commits: commits, Checks: checks})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	calls := checks.callCount()

	tests := []struct {
		name       string
		repository string
		sha        string
	}{
		{name: "untracked SHA", repository: "example-user/blog", sha: shaB},
		{name: "untracked repository", repository: "example-user/other", sha: shaA},
		{name: "app moved to another repository", repository: "example-user/blog", sha: shaA},
	}
	for _, tt := range tests {
		results, err := current.Reevaluate(t.Context(), tt.repository, tt.sha)
		if err != nil || results != nil {
			t.Errorf("%s: Reevaluate = %+v, %v, want nothing", tt.name, results, err)
		}
	}
	if checks.callCount() != calls {
		t.Errorf("Reevaluate of untracked commits made %d Checks calls", checks.callCount()-calls)
	}
	if _, err := current.Reevaluate(t.Context(), "example-user/blog", "main"); !errors.Is(err, core.ErrInvalidCommitSHA) {
		t.Errorf("Reevaluate with a branch name error = %v, want ErrInvalidCommitSHA", err)
	}
}

func TestCheckSourceErrorLeavesCommitPending(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod"))
	h.checks.fail(github.ErrNotInstalled)
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

func TestEnqueueErrorLeavesCommitUnenqueued(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod"))
	h.checks.set("example-user/blog", shaA, green("test"))
	h.commits.queue.fail(errBoom)

	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, errBoom) {
		t.Fatalf("HandlePush error = %v, want %v", err, errBoom)
	}
	if len(results) != 1 || results[0].Enqueued || results[0].Commit.DeploymentID != nil || results[0].Commit.ChecksState != core.ChecksSuccess {
		t.Fatalf("result = %+v, want success but not enqueued", results)
	}
	if got := h.commits.get(results[0].Commit.ID); !got.Open() {
		t.Errorf("commit closed after failed enqueue: %+v", got)
	}

	h.commits.queue.fail(nil)
	if results := h.reevaluate(t, shaA); len(results) != 1 || !results[0].Enqueued {
		t.Errorf("Reevaluate after queue recovered = %+v, want enqueued", results)
	}
}

func TestErrorsForOneAppDoNotBlockAnother(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("staging"), blogApp("prod"))
	h.commits.trackErr[blogApp("staging").Target] = errBoom
	h.checks.set("example-user/blog", shaA, green("test"))

	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "blog-staging/staging") {
		t.Errorf("HandlePush error = %v, want the staging failure", err)
	}
	if len(results) != 1 || results[0].Commit.Target != blogApp("prod").Target || !results[0].Enqueued {
		t.Errorf("HandlePush = %+v, want prod enqueued", results)
	}
}

func TestSaveChecksErrorIsReturned(t *testing.T) {
	t.Parallel()

	h := newHarness(t, blogApp("prod"))
	h.checks.set("example-user/blog", shaA, green("test"))
	h.commits.saveErr = errBoom
	results, err := h.svc.HandlePush(t.Context(), push(shaA))
	if !errors.Is(err, errBoom) {
		t.Errorf("HandlePush error = %v, want %v", err, errBoom)
	}
	if len(results) != 1 || results[0].Enqueued {
		t.Errorf("HandlePush = %+v, want one unenqueued result", results)
	}
	if n := len(h.commits.queue.all()); n != 0 {
		t.Errorf("enqueued %d deployments after failing to save checks", n)
	}
}
