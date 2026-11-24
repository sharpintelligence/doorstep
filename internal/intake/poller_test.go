package intake_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

var shaC = strings.Repeat("c", 40)

func apiApp() intake.App {
	return intake.App{
		Target:          core.AppTarget("api"),
		Repository:      "example-user/api",
		Branch:          "trunk",
		ImageRepository: "ghcr.io/example-user/api",
	}
}

type pollerHarness struct {
	intake   *fakeIntake
	branches *fakeBranches
	claims   *fakeClaims
	observer *fakeObserver
	clock    *fixedClock
	poller   *intake.Poller
}

type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Now() time.Time { return c.now }

func newPollerHarness(t *testing.T, apps ...intake.App) *pollerHarness {
	t.Helper()
	h := &pollerHarness{
		intake:   &fakeIntake{},
		branches: newFakeBranches(),
		claims:   &fakeClaims{},
		observer: &fakeObserver{},
		clock:    &fixedClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)},
	}
	poller, err := intake.NewPoller(intake.PollerOptions{
		Apps:     apps,
		Intake:   h.intake,
		Branches: h.branches,
		Claims:   h.claims,
		Interval: time.Minute,
		Observer: h.observer,
		Now:      h.clock.Now,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	h.poller = poller
	return h
}

func polledPush(repository, branch, sha string) intake.Push {
	return intake.Push{Repository: repository, Branch: branch, SHA: sha, Source: core.SourcePoll, PushedBy: intake.PollerActor}
}

func openCommit(target core.Target, repository, sha string, evaluatedAt *time.Time) core.TrackedCommit {
	return core.TrackedCommit{
		Target:      target,
		Commit:      core.Commit{Repository: repository, SHA: sha},
		Branch:      "main",
		ChecksState: core.ChecksPending,
		EvaluatedAt: evaluatedAt,
	}
}

func TestPollerReadsEachBranchOnce(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(t, blogApp("staging"), blogApp("prod"), apiApp(), intake.App{
		Target: core.AppTarget("blog-preview"), Repository: "example-user/BLOG", Branch: "preview", ImageRepository: "ghcr.io/example-user/blog",
	})
	h.branches.set("example-user/blog", "main", shaA)
	h.branches.set("example-user/api", "trunk", shaB)
	h.branches.set("example-user/blog", "preview", shaC)

	if err := h.poller.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	wantReads := []string{"example-user/blog:main", "example-user/api:trunk", "example-user/blog:preview"}
	if reads := h.branches.readKeys(); !slices.Equal(reads, wantReads) {
		t.Errorf("branch reads = %v, want %v", reads, wantReads)
	}
	pushes, _, _ := h.intake.recorded()
	wantPushes := []intake.Push{
		polledPush("Example-User/blog", "main", shaA),
		polledPush("example-user/api", "trunk", shaB),
		polledPush("example-user/BLOG", "preview", shaC),
	}
	if !slices.Equal(pushes, wantPushes) {
		t.Errorf("pushes = %+v, want %+v", pushes, wantPushes)
	}
	if h.claims.every != time.Minute {
		t.Errorf("claim interval = %s, want 1m", h.claims.every)
	}
	if _, cycles := h.observer.recorded(); !slices.Equal(cycles, []intake.PollResult{intake.PollSucceeded}) {
		t.Errorf("cycles = %v, want one success", cycles)
	}
}

func TestPollerHandlesOnlyChangedHeads(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(t, blogApp("prod"))
	steps := []struct {
		name       string
		head       string
		wantPushes []intake.Push
	}{
		{name: "first cycle handles the head", head: shaA, wantPushes: []intake.Push{polledPush("Example-User/blog", "main", shaA)}},
		{name: "unchanged head is not handled again", head: shaA},
		{name: "new head is handled", head: shaB, wantPushes: []intake.Push{polledPush("Example-User/blog", "main", shaB)}},
		{name: "force-push back is handled", head: shaA, wantPushes: []intake.Push{polledPush("Example-User/blog", "main", shaA)}},
		{name: "deleted branch is skipped", head: ""},
		{name: "recreated branch is handled", head: shaC, wantPushes: []intake.Push{polledPush("Example-User/blog", "main", shaC)}},
	}
	for _, step := range steps {
		h.intake.reset()
		h.branches.set("example-user/blog", "main", step.head)
		if err := h.poller.RunOnce(t.Context()); err != nil {
			t.Fatalf("%s: RunOnce: %v", step.name, err)
		}
		if pushes, _, _ := h.intake.recorded(); !slices.Equal(pushes, step.wantPushes) {
			t.Errorf("%s: pushes = %+v, want %+v", step.name, pushes, step.wantPushes)
		}
	}
}

func TestPollerRetriesHeadAfterFailure(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(t, blogApp("prod"))
	h.branches.set("example-user/blog", "main", shaA)
	h.intake.failPushes(errBoom)
	if err := h.poller.RunOnce(t.Context()); !errors.Is(err, errBoom) {
		t.Fatalf("RunOnce error = %v, want boom", err)
	}
	h.intake.failPushes(nil)
	if err := h.poller.RunOnce(t.Context()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if err := h.poller.RunOnce(t.Context()); err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	pushes, _, _ := h.intake.recorded()
	if want := slices.Repeat([]intake.Push{polledPush("Example-User/blog", "main", shaA)}, 2); !slices.Equal(pushes, want) {
		t.Errorf("pushes = %+v, want %+v", pushes, want)
	}
	if _, cycles := h.observer.recorded(); !slices.Equal(cycles, []intake.PollResult{intake.PollFailed, intake.PollSucceeded, intake.PollSucceeded}) {
		t.Errorf("cycles = %v", cycles)
	}
}

func TestPollerBranchErrorDoesNotStopTheCycle(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(t, blogApp("prod"), apiApp())
	h.branches.failFor("example-user/blog", "main", errBoom)
	h.branches.set("example-user/api", "trunk", shaB)
	h.intake.setOpen(openCommit(core.AppTarget("api"), "example-user/api", shaC, nil))

	err := h.poller.RunOnce(t.Context())
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "read head of Example-User/blog branch main") {
		t.Fatalf("RunOnce error = %v, want the branch error", err)
	}
	pushes, reevaluations, _ := h.intake.recorded()
	if want := []intake.Push{polledPush("example-user/api", "trunk", shaB)}; !slices.Equal(pushes, want) {
		t.Errorf("pushes = %+v, want %+v", pushes, want)
	}
	if want := []reevaluation{{repository: "example-user/api", sha: shaC}}; !slices.Equal(reevaluations, want) {
		t.Errorf("reevaluations = %+v, want %+v", reevaluations, want)
	}
	if _, cycles := h.observer.recorded(); !slices.Equal(cycles, []intake.PollResult{intake.PollFailed}) {
		t.Errorf("cycles = %v, want one failure", cycles)
	}
}

func TestPollerReevaluatesOpenCommits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	at := func(ago time.Duration) *time.Time {
		t := now.Add(-ago)
		return &t
	}
	staging, prod := blogApp("staging").Target, blogApp("prod").Target
	tests := []struct {
		name              string
		head              string
		open              []core.TrackedCommit
		wantReevaluations []reevaluation
	}{
		{
			name: "never evaluated",
			open: []core.TrackedCommit{openCommit(prod, "Example-User/blog", shaB, nil)},
			wantReevaluations: []reevaluation{
				{repository: "Example-User/blog", sha: shaB},
			},
		},
		{
			name: "evaluated a poll interval ago",
			open: []core.TrackedCommit{openCommit(prod, "Example-User/blog", shaB, at(time.Minute))},
			wantReevaluations: []reevaluation{
				{repository: "Example-User/blog", sha: shaB},
			},
		},
		{
			name: "evaluated just over half an interval ago",
			open: []core.TrackedCommit{openCommit(prod, "Example-User/blog", shaB, at(31*time.Second))},
			wantReevaluations: []reevaluation{
				{repository: "Example-User/blog", sha: shaB},
			},
		},
		{
			name: "recently evaluated by a webhook",
			open: []core.TrackedCommit{openCommit(prod, "Example-User/blog", shaB, at(10*time.Second))},
		},
		{
			name: "one evaluation per repository and SHA",
			open: []core.TrackedCommit{
				openCommit(staging, "Example-User/blog", shaB, nil),
				openCommit(prod, "example-user/blog", shaB, nil),
			},
			wantReevaluations: []reevaluation{
				{repository: "Example-User/blog", sha: shaB},
			},
		},
		{
			name: "head handled this cycle is not evaluated again",
			head: shaA,
			open: []core.TrackedCommit{
				openCommit(prod, "example-user/blog", shaA, nil),
				openCommit(staging, "Example-User/blog", shaC, nil),
			},
			wantReevaluations: []reevaluation{
				{repository: "Example-User/blog", sha: shaC},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newPollerHarness(t, blogApp("staging"), blogApp("prod"))
			h.clock.now = now
			h.branches.set("example-user/blog", "main", tt.head)
			h.intake.setOpen(tt.open...)
			if err := h.poller.RunOnce(t.Context()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if _, reevaluations, _ := h.intake.recorded(); !slices.Equal(reevaluations, tt.wantReevaluations) {
				t.Errorf("reevaluations = %+v, want %+v", reevaluations, tt.wantReevaluations)
			}
		})
	}
}

func TestPollerOpenLimit(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(t, blogApp("prod"))
	var open []core.TrackedCommit
	for i := range intake.PollOpenLimit + 5 {
		open = append(open, openCommit(blogApp("prod").Target, "Example-User/blog", fmt.Sprintf("%040x", i+1), nil))
	}
	h.intake.setOpen(open...)
	if err := h.poller.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, reevaluations, _ := h.intake.recorded(); len(reevaluations) != intake.PollOpenLimit {
		t.Errorf("reevaluations = %d, want %d", len(reevaluations), intake.PollOpenLimit)
	}
}

func TestPollerErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(*pollerHarness)
		wantErr    string
		wantCycles []intake.PollResult
	}{
		{
			name: "open fails",
			setup: func(h *pollerHarness) {
				h.intake.mu.Lock()
				h.intake.openErr = errBoom
				h.intake.mu.Unlock()
			},
			wantErr:    "list open commits: boom",
			wantCycles: []intake.PollResult{intake.PollFailed},
		},
		{
			name: "re-evaluation fails",
			setup: func(h *pollerHarness) {
				h.intake.setOpen(openCommit(blogApp("prod").Target, "Example-User/blog", shaB, nil))
				h.intake.mu.Lock()
				h.intake.reevaluateErr = errBoom
				h.intake.mu.Unlock()
			},
			wantErr:    "re-evaluate Example-User/blog@" + shaB + ": boom",
			wantCycles: []intake.PollResult{intake.PollFailed},
		},
		{
			name:    "claim fails",
			setup:   func(h *pollerHarness) { h.claims.err = errBoom },
			wantErr: "claim intake poll: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newPollerHarness(t, blogApp("prod"))
			tt.setup(h)
			err := h.poller.RunOnce(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("RunOnce error = %v, want %q", err, tt.wantErr)
			}
			if _, cycles := h.observer.recorded(); !slices.Equal(cycles, tt.wantCycles) {
				t.Errorf("cycles = %v, want %v", cycles, tt.wantCycles)
			}
		})
	}
}

func TestPollerSkipsCycleWhenAnotherReplicaClaimedIt(t *testing.T) {
	t.Parallel()

	h := newPollerHarness(t, blogApp("prod"))
	h.claims.refuse = true
	h.branches.set("example-user/blog", "main", shaA)
	h.intake.setOpen(openCommit(blogApp("prod").Target, "Example-User/blog", shaA, nil))
	if err := h.poller.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if h.branches.calls != 0 {
		t.Errorf("branch reads = %d, want 0", h.branches.calls)
	}
	pushes, reevaluations, _ := h.intake.recorded()
	if len(pushes)+len(reevaluations) != 0 || h.intake.openCalls != 0 {
		t.Errorf("intake called: pushes %v, reevaluations %v, open %d", pushes, reevaluations, h.intake.openCalls)
	}
	if _, cycles := h.observer.recorded(); len(cycles) != 0 {
		t.Errorf("cycles = %v, want none", cycles)
	}
}

func TestPollerRunTicksAndStops(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		interval  time.Duration
		wait      time.Duration
		wantTick  time.Duration
		wantCalls int
	}{
		{name: "long interval ticks every 15s", interval: time.Minute, wait: 45 * time.Second, wantTick: 15 * time.Second, wantCalls: 4},
		{name: "short interval ticks at the interval", interval: 10 * time.Second, wait: 30 * time.Second, wantTick: 10 * time.Second, wantCalls: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				claims := &fakeClaims{refuse: true}
				poller, err := intake.NewPoller(intake.PollerOptions{
					Apps:     []intake.App{blogApp("prod")},
					Intake:   &fakeIntake{},
					Branches: newFakeBranches(),
					Claims:   claims,
					Interval: tt.interval,
				})
				if err != nil {
					t.Fatalf("NewPoller: %v", err)
				}
				if got := poller.Tick(); got != tt.wantTick {
					t.Errorf("Tick() = %s, want %s", got, tt.wantTick)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- poller.Run(ctx) }()

				time.Sleep(tt.wait)
				synctest.Wait()
				if got := claims.callCount(); got != tt.wantCalls {
					t.Errorf("claims after %s = %d, want %d", tt.wait, got, tt.wantCalls)
				}
				cancel()
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("Run = %v, want nil", err)
					}
				default:
					t.Fatal("Run did not return after cancellation")
				}
			})
		})
	}
}

func TestPollerRunKeepsGoingAfterErrors(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		claims := &fakeClaims{err: errBoom}
		poller, err := intake.NewPoller(intake.PollerOptions{
			Intake:   &fakeIntake{},
			Branches: newFakeBranches(),
			Claims:   claims,
			Interval: time.Minute,
		})
		if err != nil {
			t.Fatalf("NewPoller: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = poller.Run(ctx) }()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if got := claims.callCount(); got != 3 {
			t.Errorf("claims = %d, want 3", got)
		}
	})
}

func TestNewPollerValidatesOptions(t *testing.T) {
	t.Parallel()

	valid := func() intake.PollerOptions {
		return intake.PollerOptions{Intake: &fakeIntake{}, Branches: newFakeBranches(), Claims: &fakeClaims{}, Interval: time.Minute}
	}
	tests := []struct {
		name    string
		mutate  func(*intake.PollerOptions)
		wantErr string
	}{
		{name: "valid", mutate: func(*intake.PollerOptions) {}},
		{name: "no intake", mutate: func(o *intake.PollerOptions) { o.Intake = nil }, wantErr: "intake is required"},
		{name: "no branches", mutate: func(o *intake.PollerOptions) { o.Branches = nil }, wantErr: "branch source is required"},
		{name: "no claims", mutate: func(o *intake.PollerOptions) { o.Claims = nil }, wantErr: "claims are required"},
		{name: "zero interval", mutate: func(o *intake.PollerOptions) { o.Interval = 0 }, wantErr: "poll interval must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := valid()
			tt.mutate(&opts)
			_, err := intake.NewPoller(opts)
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("NewPoller error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
