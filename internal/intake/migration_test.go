package intake_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

var (
	shaBase = strings.Repeat("0", 39) + "1"
	shaMid  = strings.Repeat("c", 40)
)

type fakeMigrations struct {
	mu         sync.Mutex
	ranges     map[string]github.CommitRange
	rangeErr   error
	commits    map[string]github.CommitInfo
	commitErr  error
	pulls      map[string][]github.CommitPullRequest
	pullsErr   error
	rangeCalls []string
	commitRead int
	pullsRead  int
}

func newFakeMigrations() *fakeMigrations {
	return &fakeMigrations{ranges: map[string]github.CommitRange{}, commits: map[string]github.CommitInfo{}, pulls: map[string][]github.CommitPullRequest{}}
}

func (f *fakeMigrations) Range(_ context.Context, repository, base, head string, limit int) (github.CommitRange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rangeCalls = append(f.rangeCalls, fmt.Sprintf("%s %s...%s %d", strings.ToLower(repository), base[:4], head[:4], limit))
	if f.rangeErr != nil {
		return github.CommitRange{}, f.rangeErr
	}
	r, ok := f.ranges[base+"..."+head]
	if !ok {
		return github.CommitRange{}, fmt.Errorf("compare: %w", github.ErrCommitNotFound)
	}
	return r, nil
}

func (f *fakeMigrations) Commit(_ context.Context, _ string, sha string) (github.CommitInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitRead++
	if f.commitErr != nil {
		return github.CommitInfo{}, f.commitErr
	}
	c, ok := f.commits[sha]
	if !ok {
		return github.CommitInfo{SHA: sha, Message: "plain commit"}, nil
	}
	return c, nil
}

func (f *fakeMigrations) PullRequests(_ context.Context, _ string, sha string) ([]github.CommitPullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullsRead++
	if f.pullsErr != nil {
		return nil, f.pullsErr
	}
	return f.pulls[sha], nil
}

func (f *fakeMigrations) counts() (ranges, commits, pulls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rangeCalls), f.commitRead, f.pullsRead
}

type fakeBaselines struct {
	mu      sync.Mutex
	commits map[core.Target]core.Commit
	err     error
}

func (f *fakeBaselines) LastDeployCommit(_ context.Context, t core.Target) (core.Commit, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return core.Commit{}, false, f.err
	}
	c, ok := f.commits[t]
	return c, ok, nil
}

type migrationHarness struct {
	*harness
	source    *fakeMigrations
	baselines *fakeBaselines
}

func newMigrationHarness(t *testing.T, apps ...intake.App) *migrationHarness {
	t.Helper()
	h := &migrationHarness{
		harness:   &harness{commits: newFakeCommits(), checks: newFakeChecks(), branches: newFakeBranches()},
		source:    newFakeMigrations(),
		baselines: &fakeBaselines{commits: map[core.Target]core.Commit{}},
	}
	svc, err := intake.New(intake.Options{
		Apps: apps, Commits: h.commits, Checks: h.checks, Branches: h.branches,
		Migrations: h.source, Baselines: h.baselines, Now: newTickingClock().Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.svc = svc
	return h
}

func migratingApp(env string, policy intake.MigrationPolicy) intake.App {
	app := blogApp(env)
	app.Migrations = &policy
	return app
}

func labelsAndTrailer() intake.MigrationPolicy {
	return intake.MigrationPolicy{Labels: []string{"db-migration"}, Trailer: true}
}

func mergedPull(number int, labels ...string) github.CommitPullRequest {
	return github.CommitPullRequest{Number: number, Merged: true, BaseRef: "main", Labels: labels}
}

func TestMigrationFlagReachesEnqueuedDeployment(t *testing.T) {
	t.Parallel()

	prod := core.Target{App: "blog-prod", Environment: "prod"}
	tests := []struct {
		name        string
		policy      intake.MigrationPolicy
		setup       func(h *migrationHarness)
		wantFlag    bool
		wantReason  string
		wantRanges  int
		wantCommits int
		wantPulls   int
	}{
		{
			name:   "label on a merged pull request in the middle of the range",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "example-user/BLOG", SHA: shaBase}
				h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonAhead, Total: 2, Commits: []github.CommitInfo{{SHA: shaMid}, {SHA: shaA}}}
				h.source.pulls[shaMid] = []github.CommitPullRequest{mergedPull(12, "DB-Migration")}
			},
			wantFlag: true, wantReason: "label DB-Migration on #12 (ccccccc)", wantRanges: 1, wantPulls: 2,
		},
		{
			name:   "trailer in the range without labels configured",
			policy: intake.MigrationPolicy{Trailer: true},
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
				h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonAhead, Total: 2, Commits: []github.CommitInfo{
					{SHA: shaMid, Message: "add column\n\nDoorstep-Migration: true"}, {SHA: shaA},
				}}
			},
			wantFlag: true, wantReason: "trailer Doorstep-Migration in ccccccc", wantRanges: 1,
		},
		{
			name:   "diverged range is inspected",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
				h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonDiverged, Total: 1, Commits: []github.CommitInfo{{SHA: shaA, Message: "Doorstep-Migration: yes"}}}
			},
			wantFlag: true, wantReason: "trailer Doorstep-Migration in aaaaaaa", wantRanges: 1, wantPulls: 1,
		},
		{
			name:   "unflagged range",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
				h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonAhead, Total: 2, Commits: []github.CommitInfo{{SHA: shaMid}, {SHA: shaA}}}
				h.source.pulls[shaMid] = []github.CommitPullRequest{{Number: 3, BaseRef: "main", Labels: []string{"db-migration"}}}
			},
			wantRanges: 1, wantPulls: 2,
		},
		{
			name:   "too many commits are flagged without reading pull requests",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
				h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonAhead, Total: 52, Commits: []github.CommitInfo{{SHA: shaA}}}
			},
			wantFlag: true, wantReason: "52 commits since the last release, too many to inspect", wantRanges: 1,
		},
		{
			name:   "no baseline inspects the head commit only",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.source.pulls[shaA] = []github.CommitPullRequest{mergedPull(4, "db-migration")}
			},
			wantFlag: true, wantReason: "label db-migration on #4 (aaaaaaa)", wantCommits: 1, wantPulls: 1,
		},
		{
			name:   "baseline is the head itself",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaA}
				h.source.commits[shaA] = github.CommitInfo{SHA: shaA, Message: "Doorstep-Migration: true"}
			},
			wantFlag: true, wantReason: "trailer Doorstep-Migration in aaaaaaa", wantCommits: 1, wantPulls: 1,
		},
		{
			name:   "baseline from another repository",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "example-user/old-blog", SHA: shaBase}
			},
			wantCommits: 1, wantPulls: 1,
		},
		{
			name:   "head behind the baseline",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
				h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonBehind}
				h.source.commits[shaA] = github.CommitInfo{SHA: shaA, Message: "revert\n\nDoorstep-Migration: true"}
			},
			wantFlag: true, wantReason: "trailer Doorstep-Migration in aaaaaaa", wantRanges: 1, wantCommits: 1, wantPulls: 1,
		},
		{
			name:   "identical range",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
				h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonIdentical}
			},
			wantRanges: 1, wantCommits: 1, wantPulls: 1,
		},
		{
			name:   "baseline unknown to GitHub",
			policy: labelsAndTrailer(),
			setup: func(h *migrationHarness) {
				h.baselines.commits[prod] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
				h.source.pulls[shaA] = []github.CommitPullRequest{mergedPull(9, "db-migration")}
			},
			wantFlag: true, wantReason: "label db-migration on #9 (aaaaaaa)", wantRanges: 1, wantCommits: 1, wantPulls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newMigrationHarness(t, migratingApp("prod", tt.policy))
			h.checks.set("example-user/blog", shaA, green("test"))
			tt.setup(h)

			results := h.push(t, push(shaA))
			if len(results) != 1 || !results[0].Enqueued {
				t.Fatalf("HandlePush = %+v, want one enqueued result", results)
			}
			deployments := h.commits.queue.all()
			if len(deployments) != 1 {
				t.Fatalf("enqueued %d deployments, want 1", len(deployments))
			}
			d := deployments[0]
			if d.Migration != tt.wantFlag || d.MigrationReason != tt.wantReason {
				t.Errorf("deployment migration = %v %q, want %v %q", d.Migration, d.MigrationReason, tt.wantFlag, tt.wantReason)
			}
			ranges, commits, pulls := h.source.counts()
			if ranges != tt.wantRanges || commits != tt.wantCommits || pulls != tt.wantPulls {
				t.Errorf("GitHub reads = %d ranges, %d commits, %d pull lists; want %d, %d, %d", ranges, commits, pulls, tt.wantRanges, tt.wantCommits, tt.wantPulls)
			}
		})
	}
}

func TestMigrationDetectionFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy intake.MigrationPolicy
		fail   func(h *migrationHarness, err error)
	}{
		{name: "baseline read", policy: labelsAndTrailer(), fail: func(h *migrationHarness, err error) { h.baselines.err = err }},
		{name: "range read", policy: labelsAndTrailer(), fail: func(h *migrationHarness, err error) {
			h.baselines.commits[core.Target{App: "blog-prod", Environment: "prod"}] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
			h.source.rangeErr = err
		}},
		{name: "commit read", policy: intake.MigrationPolicy{Trailer: true}, fail: func(h *migrationHarness, err error) { h.source.commitErr = err }},
		{name: "pull request read", policy: labelsAndTrailer(), fail: func(h *migrationHarness, err error) { h.source.pullsErr = err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newMigrationHarness(t, migratingApp("prod", tt.policy))
			h.checks.set("example-user/blog", shaA, green("test"))
			tt.fail(h, errBoom)
			h.branches.set("example-user/blog", "main", shaA)

			results, err := h.svc.HandlePush(t.Context(), push(shaA))
			if !errors.Is(err, errBoom) {
				t.Fatalf("HandlePush error = %v, want %v", err, errBoom)
			}
			if len(results) != 1 || results[0].Enqueued {
				t.Fatalf("HandlePush = %+v, want one unenqueued result", results)
			}
			got := h.commits.get(results[0].Commit.ID)
			if !got.Open() || got.EvaluatedAt != nil {
				t.Errorf("tracked commit = %+v, want open and unsettled", got)
			}
			if n := len(h.commits.queue.all()); n != 0 {
				t.Errorf("enqueued %d deployments while migration status was unknown", n)
			}

			tt.fail(h, nil)
			if results := h.poll(t); len(results) != 1 || !results[0].Enqueued {
				t.Errorf("poll after GitHub recovered = %+v, want enqueued", results)
			}
		})
	}
}

func TestMigrationDetectionOnlyForGreenCommitsOfOptedInApps(t *testing.T) {
	t.Parallel()

	plain := blogApp("staging")
	h := newMigrationHarness(t, plain, migratingApp("prod", labelsAndTrailer()))
	h.checks.set("example-user/blog", shaA, intake.Snapshot{CheckRuns: []github.CheckRun{{ID: 1, Name: "test", Status: "in_progress"}}})
	h.push(t, push(shaA))
	if ranges, commits, pulls := h.source.counts(); ranges+commits+pulls != 0 {
		t.Fatalf("pending commit read migrations: %d ranges, %d commits, %d pull lists", ranges, commits, pulls)
	}

	h.checks.set("example-user/blog", shaA, green("test"))
	results := h.reevaluate(t, shaA)
	if len(results) != 2 || !results[0].Enqueued || !results[1].Enqueued {
		t.Fatalf("Reevaluate = %+v, want both enqueued", results)
	}
	if _, commits, pulls := h.source.counts(); commits != 1 || pulls != 1 {
		t.Errorf("GitHub reads = %d commits, %d pull lists; want one each for the opted-in app", commits, pulls)
	}
	for _, d := range h.commits.queue.all() {
		if d.Migration {
			t.Errorf("deployment %+v is flagged", d)
		}
	}
}

func TestMigrationReadsAreSharedWithinOnePass(t *testing.T) {
	t.Parallel()

	h := newMigrationHarness(t, migratingApp("staging", labelsAndTrailer()), migratingApp("prod", labelsAndTrailer()))
	for _, env := range []string{"staging", "prod"} {
		h.baselines.commits[core.Target{App: "blog-" + env, Environment: env}] = core.Commit{Repository: "Example-User/blog", SHA: shaBase}
	}
	h.source.ranges[shaBase+"..."+shaA] = github.CommitRange{Status: github.ComparisonAhead, Total: 2, Commits: []github.CommitInfo{{SHA: shaMid}, {SHA: shaA}}}
	h.source.pulls[shaMid] = []github.CommitPullRequest{mergedPull(5, "db-migration")}
	h.checks.set("example-user/blog", shaA, green("test"))

	results := h.push(t, push(shaA))
	if len(results) != 2 || !results[0].Enqueued || !results[1].Enqueued {
		t.Fatalf("HandlePush = %+v, want both enqueued", results)
	}
	if ranges, commits, pulls := h.source.counts(); ranges != 1 || commits != 0 || pulls != 2 {
		t.Errorf("GitHub reads = %d ranges, %d commits, %d pull lists; want 1, 0, 2", ranges, commits, pulls)
	}
	for _, d := range h.commits.queue.all() {
		if !d.Migration || d.MigrationReason != "label db-migration on #5 (ccccccc)" {
			t.Errorf("deployment %s migration = %v %q", d.Target, d.Migration, d.MigrationReason)
		}
	}
}

func TestNewRequiresMigrationPorts(t *testing.T) {
	t.Parallel()

	app := migratingApp("prod", labelsAndTrailer())
	tests := []struct {
		name string
		opts intake.Options
	}{
		{name: "no source", opts: intake.Options{Baselines: &fakeBaselines{}}},
		{name: "no baselines", opts: intake.Options{Migrations: newFakeMigrations()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := tt.opts
			opts.Apps, opts.Commits, opts.Checks, opts.Branches = []intake.App{app}, newFakeCommits(), newFakeChecks(), newFakeBranches()
			if _, err := intake.New(opts); err == nil || !strings.Contains(err.Error(), "migration detection needs") {
				t.Errorf("New error = %v, want a missing port error", err)
			}
		})
	}

	invalid := migratingApp("prod", intake.MigrationPolicy{})
	_, err := intake.New(intake.Options{Apps: []intake.App{invalid}, Commits: newFakeCommits(), Checks: newFakeChecks(), Branches: newFakeBranches(), Migrations: newFakeMigrations(), Baselines: &fakeBaselines{}})
	if err == nil || !strings.Contains(err.Error(), "at least one label or the trailer") {
		t.Errorf("New with an empty policy error = %v", err)
	}
}
