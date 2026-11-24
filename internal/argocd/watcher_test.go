package argocd

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/argocd/argocdtest"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/kube"
)

var (
	blogTarget = core.Target{App: "blog", Environment: "blog"}
	shopTarget = core.Target{App: "shop", Environment: "shop"}
)

type fakeAncestry struct {
	mu     sync.Mutex
	answer map[string]bool
	errs   []error
	calls  map[string]int
}

func (f *fakeAncestry) Contains(_ context.Context, repo, ancestor, descendant string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if repo != repository || ancestor != pinSHA {
		return false, errors.New("unexpected ancestry question")
	}
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[descendant]++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return false, err
	}
	return f.answer[descendant], nil
}

func (f *fakeAncestry) callsFor(revision string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[revision]
}

type fixture struct {
	srv      *argocdtest.Server
	watcher  *Watcher
	ancestry *fakeAncestry
}

func newFixture(t *testing.T, configure func(*Options)) *fixture {
	t.Helper()
	srv := argocdtest.NewServer(t)
	client, err := kube.New(kube.Config{Server: srv.URL}, kube.Options{})
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	ancestry := &fakeAncestry{}
	opts := Options{
		Client: client,
		Applications: map[core.Target]Application{
			blogTarget: {Namespace: "argocd", Name: "blog", Repository: repository, SyncTimeout: 10 * time.Second},
			shopTarget: {Namespace: "apps", Name: "shop", Repository: repository, SyncTimeout: 10 * time.Second},
		},
		Ancestry:       ancestry,
		ResyncInterval: time.Second,
		Wait: func(ctx context.Context, d time.Duration) error {
			return sleep(ctx, min(d, time.Millisecond))
		},
	}
	if configure != nil {
		configure(&opts)
	}
	w, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &fixture{srv: srv, watcher: w, ancestry: ancestry}
}

type result struct {
	status core.SyncStatus
	err    error
}

func (f *fixture) start(ctx context.Context, target core.Target) <-chan result {
	done := make(chan result, 1)
	go func() {
		status, err := f.watcher.WaitSynced(ctx, target, pinSHA)
		done <- result{status: status, err: err}
	}()
	return done
}

func await(t *testing.T, done <-chan result) result {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("WaitSynced did not return")
		return result{}
	}
}

func setReady(a *argocdtest.Application) {
	ready := synced(pinSHA)
	*a = ready
}

func TestWaitSyncedAlreadyReady(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.srv.Set(synced(pinSHA))
	status, err := f.watcher.WaitSynced(t.Context(), blogTarget, pinSHA)
	if err != nil || !status.Ready() || status.Revision != pinSHA {
		t.Fatalf("WaitSynced = %+v, %v", status, err)
	}
	if got := f.srv.Requests(argocdtest.Watch); got != 0 {
		t.Fatalf("watch requests = %d, want 0", got)
	}
}

func TestWaitSyncedThroughWatchEvents(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.srv.Set(with(synced(otherSHA), func(a *argocdtest.Application) {}))
	f.ancestry.answer = map[string]bool{otherSHA: false}
	done := f.start(t.Context(), blogTarget)

	f.srv.AwaitWatch(t, 1)
	f.srv.Update("argocd", "blog", func(a *argocdtest.Application) {
		a.Revision = pinSHA
		a.Sync = "OutOfSync"
		a.Operation = &argocdtest.Operation{Phase: "Running", Message: "syncing"}
	})
	f.srv.Update("argocd", "blog", func(a *argocdtest.Application) {
		a.Sync = "Synced"
		a.Health = "Progressing"
		a.Operation = &argocdtest.Operation{Phase: "Succeeded", Revision: pinSHA}
	})
	f.srv.Update("argocd", "blog", func(a *argocdtest.Application) { a.Health = "Healthy" })

	r := await(t, done)
	if r.err != nil || !r.status.Ready() {
		t.Fatalf("WaitSynced = %+v, %v", r.status, r.err)
	}
	if got := f.srv.Requests(argocdtest.Get); got != 1 {
		t.Fatalf("get requests = %d, want 1", got)
	}
	queries := f.srv.WatchQueries()
	if len(queries) != 1 {
		t.Fatalf("watch queries = %v", queries)
	}
	q := queries[0]
	if q.Get("watch") != "1" || q.Get("fieldSelector") != "metadata.name=blog" || q.Get("resourceVersion") == "" ||
		q.Get("timeoutSeconds") != "1" || q.Get("allowWatchBookmarks") != "true" {
		t.Fatalf("watch query = %v", q)
	}
	if got := f.ancestry.callsFor(otherSHA); got != 1 {
		t.Fatalf("ancestry calls for %s = %d, want 1", otherSHA, got)
	}
}

func TestWaitSyncedRecoversFromWatchProblems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		disrupt func(*argocdtest.Server)
	}{
		{name: "watch closed", disrupt: (*argocdtest.Server).CloseWatches},
		{name: "resource version expired", disrupt: (*argocdtest.Server).ExpireWatches},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
			done := f.start(t.Context(), blogTarget)

			f.srv.AwaitWatch(t, 1)
			tt.disrupt(f.srv)
			f.srv.AwaitWatch(t, 2)
			if got := f.srv.Requests(argocdtest.Get); got != 2 {
				t.Fatalf("get requests = %d, want 2 (one fresh read per broken watch)", got)
			}
			f.srv.Update("argocd", "blog", setReady)
			if r := await(t, done); r.err != nil || !r.status.Ready() {
				t.Fatalf("WaitSynced = %+v, %v", r.status, r.err)
			}
		})
	}
}

func TestWaitSyncedPollsWhenWatchFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
	}{
		{name: "watch forbidden", code: http.StatusForbidden},
		{name: "watch gone", code: http.StatusGone},
		{name: "watch server error", code: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
			f.srv.FailNext(argocdtest.Watch, tt.code, -1)
			done := f.start(t.Context(), blogTarget)

			deadline := time.Now().Add(5 * time.Second)
			for f.srv.Requests(argocdtest.Get) < 3 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			f.srv.Update("argocd", "blog", setReady)
			if r := await(t, done); r.err != nil || !r.status.Ready() {
				t.Fatalf("WaitSynced = %+v, %v", r.status, r.err)
			}
			if got := f.srv.Requests(argocdtest.Get); got < 3 {
				t.Fatalf("get requests = %d, want polling", got)
			}
		})
	}
}

func TestWaitSyncedWatchFailureWaits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
		want func(time.Duration) bool
	}{
		{name: "forbidden waits the resync interval", code: http.StatusForbidden, want: func(d time.Duration) bool { return d == time.Second }},
		{name: "errors back off within the resync interval", code: http.StatusInternalServerError, want: func(d time.Duration) bool { return d > 0 && d <= time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var waits []time.Duration
			f := newFixture(t, func(o *Options) {
				o.Wait = func(ctx context.Context, d time.Duration) error {
					mu.Lock()
					waits = append(waits, d)
					mu.Unlock()
					return sleep(ctx, time.Millisecond)
				}
			})
			f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
			f.srv.FailNext(argocdtest.Watch, tt.code, 3)
			done := f.start(t.Context(), blogTarget)
			f.srv.AwaitWatch(t, 4)
			f.srv.Update("argocd", "blog", setReady)
			if r := await(t, done); r.err != nil || !r.status.Ready() {
				t.Fatalf("WaitSynced = %+v, %v", r.status, r.err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(waits) != 3 {
				t.Fatalf("waits = %v, want 3", waits)
			}
			for _, d := range waits {
				if !tt.want(d) {
					t.Fatalf("waits = %v", waits)
				}
			}
		})
	}
}

func TestWaitSyncedRetriesAPIErrors(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.srv.Set(synced(pinSHA))
	f.srv.FailNext(argocdtest.Get, http.StatusInternalServerError, 3)
	status, err := f.watcher.WaitSynced(t.Context(), blogTarget, pinSHA)
	if err != nil || !status.Ready() {
		t.Fatalf("WaitSynced = %+v, %v", status, err)
	}
	if got := f.srv.Requests(argocdtest.Get); got != 4 {
		t.Fatalf("get requests = %d, want 4", got)
	}
}

func TestWaitSyncedAncestry(t *testing.T) {
	t.Parallel()

	t.Run("error then contained", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.ancestry.errs = []error{errors.New("github unavailable")}
		f.ancestry.answer = map[string]bool{laterSHA: true}
		f.srv.Set(synced(laterSHA))
		done := f.start(t.Context(), blogTarget)

		f.srv.AwaitWatch(t, 1)
		f.srv.Update("argocd", "blog", func(a *argocdtest.Application) { a.HealthMessage = "still fine" })
		r := await(t, done)
		if r.err != nil || !r.status.Ready() || r.status.Revision != laterSHA {
			t.Fatalf("WaitSynced = %+v, %v", r.status, r.err)
		}
		if got := f.ancestry.callsFor(laterSHA); got != 2 {
			t.Fatalf("ancestry calls = %d, want 2 (errors are not remembered)", got)
		}
	})

	t.Run("answers are remembered per revision", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.ancestry.answer = map[string]bool{otherSHA: false}
		f.srv.Set(synced(otherSHA))
		done := f.start(t.Context(), blogTarget)

		f.srv.AwaitWatch(t, 1)
		for i := range 3 {
			f.srv.Update("argocd", "blog", func(a *argocdtest.Application) { a.HealthMessage = strings.Repeat("x", i) })
		}
		f.srv.Update("argocd", "blog", setReady)
		if r := await(t, done); r.err != nil || !r.status.Ready() {
			t.Fatalf("WaitSynced = %+v, %v", r.status, r.err)
		}
		if got := f.ancestry.callsFor(otherSHA); got != 1 {
			t.Fatalf("ancestry calls = %d, want 1", got)
		}
	})
}

func TestWaitSyncedFailsFast(t *testing.T) {
	t.Parallel()

	failed := with(synced(pinSHA), func(a *argocdtest.Application) {
		a.Sync = "OutOfSync"
		a.Operation = &argocdtest.Operation{Phase: "Failed", Message: "hook migrate failed", Revision: pinSHA}
	})
	tests := []struct {
		name       string
		setup      func(*argocdtest.Server)
		target     core.Target
		revision   string
		wantIs     []error
		wantReason core.SyncFailureReason
		wantClass  core.FailureClass
		wantGets   int
	}{
		{
			name:       "failed sync operation",
			setup:      func(s *argocdtest.Server) { s.Set(failed) },
			target:     blogTarget,
			revision:   pinSHA,
			wantIs:     []error{core.ErrSyncFailed},
			wantReason: core.SyncReasonFailed,
			wantClass:  core.FailureVersion,
			wantGets:   1,
		},
		{
			name:       "application not found",
			setup:      func(*argocdtest.Server) {},
			target:     blogTarget,
			revision:   pinSHA,
			wantIs:     []error{core.ErrSyncTargetNotFound, kube.ErrNotFound},
			wantReason: core.SyncReasonNotFound,
			wantClass:  core.FailureConfiguration,
			wantGets:   1,
		},
		{
			name: "forbidden",
			setup: func(s *argocdtest.Server) {
				s.Set(synced(pinSHA))
				s.FailNext(argocdtest.Get, http.StatusForbidden, -1)
			},
			target:     blogTarget,
			revision:   pinSHA,
			wantIs:     []error{core.ErrSyncForbidden, kube.ErrForbidden},
			wantReason: core.SyncReasonForbidden,
			wantClass:  core.FailureConfiguration,
			wantGets:   1,
		},
		{
			name: "unauthorized",
			setup: func(s *argocdtest.Server) {
				s.Set(synced(pinSHA))
				s.RequireToken("expected")
			},
			target:     blogTarget,
			revision:   pinSHA,
			wantIs:     []error{core.ErrSyncForbidden, kube.ErrForbidden},
			wantReason: core.SyncReasonForbidden,
			wantClass:  core.FailureConfiguration,
			wantGets:   1,
		},
		{
			name:     "unknown target",
			setup:    func(*argocdtest.Server) {},
			target:   core.Target{App: "nope", Environment: "nope"},
			revision: pinSHA,
			wantIs:   []error{core.ErrSyncTargetNotFound},
		},
		{
			name:     "invalid revision",
			setup:    func(*argocdtest.Server) {},
			target:   blogTarget,
			revision: "main",
			wantIs:   []error{core.ErrInvalidCommitSHA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, nil)
			tt.setup(f.srv)
			status, err := f.watcher.WaitSynced(t.Context(), tt.target, tt.revision)
			for _, want := range tt.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("WaitSynced error = %v, want %v", err, want)
				}
			}
			if errors.Is(err, core.ErrSyncTimeout) {
				t.Errorf("WaitSynced error = %v, must not be a timeout", err)
			}
			if tt.wantReason != "" {
				if status.Failure == nil || status.Failure.Reason != tt.wantReason || status.Failure.Class != tt.wantClass {
					t.Fatalf("Failure = %+v, want %s/%s", status.Failure, tt.wantReason, tt.wantClass)
				}
			}
			if got := f.srv.Requests(argocdtest.Get); got != tt.wantGets {
				t.Errorf("get requests = %d, want %d", got, tt.wantGets)
			}
		})
	}
}

func TestWaitSyncedDeletedWhileWaiting(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
	done := f.start(t.Context(), blogTarget)
	f.srv.AwaitWatch(t, 1)
	f.srv.Delete("argocd", "blog")
	r := await(t, done)
	if !errors.Is(r.err, core.ErrSyncTargetNotFound) || r.status.Failure == nil || r.status.Failure.Reason != core.SyncReasonNotFound {
		t.Fatalf("WaitSynced = %+v, %v", r.status, r.err)
	}
}

func TestWaitSyncedTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(*argocdtest.Server)
		wantReason  core.SyncFailureReason
		wantClass   core.FailureClass
		wantMessage string
		unavailable bool
	}{
		{
			name:        "never observed",
			setup:       func(s *argocdtest.Server) { s.Set(synced(otherSHA)) },
			wantReason:  core.SyncReasonNotObserved,
			wantClass:   core.FailureTransient,
			wantMessage: "argocd: sync timed out after 200ms: synced revision " + otherSHA + " does not contain " + pinSHA + " yet",
		},
		{
			name: "degraded",
			setup: func(s *argocdtest.Server) {
				s.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Degraded"; a.HealthMessage = "crash loop" }))
			},
			wantReason:  core.SyncReasonDegraded,
			wantClass:   core.FailureVersion,
			wantMessage: "argocd: sync timed out after 200ms: application health is degraded: crash loop",
		},
		{
			name: "progressing",
			setup: func(s *argocdtest.Server) {
				s.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
			},
			wantReason:  core.SyncReasonProgressing,
			wantClass:   core.FailureUnknown,
			wantMessage: "application health is progressing",
		},
		{
			name: "errored operation keeps waiting",
			setup: func(s *argocdtest.Server) {
				s.Set(with(synced(pinSHA), func(a *argocdtest.Application) {
					a.Sync = "OutOfSync"
					a.Operation = &argocdtest.Operation{Phase: "Error", Message: "retrying", Revision: pinSHA}
				}))
			},
			wantReason:  core.SyncReasonError,
			wantClass:   core.FailureTransient,
			wantMessage: "sync operation errored: retrying",
		},
		{
			name: "manual sync app left out of sync",
			setup: func(s *argocdtest.Server) {
				s.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Sync = "OutOfSync"; a.Automated = false }))
			},
			wantReason:  core.SyncReasonOutOfSync,
			wantClass:   core.FailureConfiguration,
			wantMessage: "synced by hand",
		},
		{
			name: "api unavailable",
			setup: func(s *argocdtest.Server) {
				s.Set(synced(pinSHA))
				s.FailNext(argocdtest.Get, http.StatusServiceUnavailable, -1)
			},
			wantReason:  core.SyncReasonUnavailable,
			wantClass:   core.FailureTransient,
			wantMessage: "argocd: sync timed out after 200ms: sync status unavailable: could not read application argocd/blog",
			unavailable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(o *Options) {
				app := o.Applications[blogTarget]
				app.SyncTimeout = 200 * time.Millisecond
				o.Applications[blogTarget] = app
			})
			tt.setup(f.srv)
			f.ancestry.answer = map[string]bool{}
			status, err := f.watcher.WaitSynced(t.Context(), blogTarget, pinSHA)
			if !errors.Is(err, core.ErrSyncTimeout) {
				t.Fatalf("WaitSynced error = %v, want ErrSyncTimeout", err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("WaitSynced error = %v, must not be a context deadline", err)
			}
			if got := errors.Is(err, core.ErrSyncUnavailable); got != tt.unavailable {
				t.Fatalf("errors.Is(ErrSyncUnavailable) = %v, want %v", got, tt.unavailable)
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.wantMessage)
			}
			if status.Failure == nil || status.Failure.Reason != tt.wantReason || status.Failure.Class != tt.wantClass {
				t.Fatalf("Failure = %+v, want %s/%s", status.Failure, tt.wantReason, tt.wantClass)
			}
		})
	}
}

func TestWaitSyncedCallerContext(t *testing.T) {
	t.Parallel()

	t.Run("earlier caller deadline wins", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		status, err := f.watcher.WaitSynced(ctx, blogTarget, pinSHA)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, core.ErrSyncTimeout) {
			t.Fatalf("WaitSynced error = %v, want only context.DeadlineExceeded", err)
		}
		if status.Health.Status != core.HealthProgressing {
			t.Fatalf("status = %+v, want the last observation", status)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, nil)
		f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
		ctx, cancel := context.WithCancel(t.Context())
		done := f.start(ctx, blogTarget)
		f.srv.AwaitWatch(t, 1)
		cancel()
		if r := await(t, done); !errors.Is(r.err, context.Canceled) || errors.Is(r.err, core.ErrSyncTimeout) {
			t.Fatalf("WaitSynced error = %v, want context.Canceled", r.err)
		}
	})
}

func TestWaitSyncedConcurrentApps(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.srv.Set(with(synced(pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
	f.srv.Set(with(argocdtest.Synced("apps", "shop", repoURL, pinSHA), func(a *argocdtest.Application) { a.Health = "Progressing" }))
	blog := f.start(t.Context(), blogTarget)
	shop := f.start(t.Context(), shopTarget)

	deadline := time.Now().Add(5 * time.Second)
	for f.srv.OpenWatches() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	f.srv.Update("apps", "shop", func(a *argocdtest.Application) { a.Health = "Healthy" })
	if r := await(t, shop); r.err != nil || !r.status.Ready() {
		t.Fatalf("shop = %+v, %v", r.status, r.err)
	}
	f.srv.Update("argocd", "blog", func(a *argocdtest.Application) { a.Health = "Healthy" })
	if r := await(t, blog); r.err != nil || !r.status.Ready() {
		t.Fatalf("blog = %+v, %v", r.status, r.err)
	}
}

func TestHealthCheckAndStatus(t *testing.T) {
	t.Parallel()

	f := newFixture(t, nil)
	f.ancestry.answer = map[string]bool{laterSHA: true}
	f.srv.Set(with(synced(laterSHA), func(a *argocdtest.Application) {
		a.Health = "Degraded"
		a.HealthMessage = "one pod failing"
		a.Resources = []argocdtest.Resource{
			{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web", Status: "Synced", Health: "Degraded", HealthMessage: "crash loop"},
			{Kind: "Service", Namespace: "blog", Name: "web", Status: "Synced", Health: "Healthy"},
		}
	}))

	health, err := f.watcher.Health(t.Context(), blogTarget)
	if err != nil || health.Status != core.HealthDegraded || health.Message != "one pod failing" || len(health.Resources) != 1 || health.ObservedAt.IsZero() {
		t.Fatalf("Health = %+v, %v", health, err)
	}

	check, err := f.watcher.Check(t.Context(), blogTarget, pinSHA)
	if err != nil || !check.Reached || check.Ready() || check.Failure == nil || check.Failure.Reason != core.SyncReasonDegraded {
		t.Fatalf("Check = %+v, %v", check, err)
	}
	if got := f.srv.Requests(argocdtest.Watch); got != 0 {
		t.Fatalf("Check opened %d watches", got)
	}

	status, err := f.watcher.Status(t.Context(), blogTarget)
	if err != nil || status.Reached || status.Failure != nil || status.Revision != laterSHA {
		t.Fatalf("Status = %+v, %v", status, err)
	}

	if _, err := f.watcher.Check(t.Context(), blogTarget, "HEAD"); !errors.Is(err, core.ErrInvalidCommitSHA) {
		t.Fatalf("Check(HEAD) error = %v", err)
	}
	if _, err := f.watcher.Health(t.Context(), shopTarget); !errors.Is(err, core.ErrSyncTargetNotFound) {
		t.Fatalf("Health(missing) error = %v", err)
	}
	if _, err := f.watcher.Health(t.Context(), core.Target{App: "x", Environment: "x"}); !errors.Is(err, core.ErrSyncTargetNotFound) {
		t.Fatalf("Health(unknown) error = %v", err)
	}
	f.srv.FailNext(argocdtest.Get, http.StatusInternalServerError, 1)
	if _, err := f.watcher.Health(t.Context(), blogTarget); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Health(500) error = %v", err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	client, err := kube.New(kube.Config{Server: "http://127.0.0.1:1"}, kube.Options{})
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	valid := Application{Namespace: "argocd", Name: "blog", Repository: repository}
	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{name: "defaults", opts: Options{Client: client, Applications: map[core.Target]Application{blogTarget: valid}}},
		{name: "no client", opts: Options{}, wantErr: "client is required"},
		{name: "negative resync", opts: Options{Client: client, ResyncInterval: -time.Second}, wantErr: "resync interval"},
		{name: "invalid target", opts: Options{Client: client, Applications: map[core.Target]Application{{App: "blog"}: valid}}, wantErr: "environment"},
		{name: "no name", opts: Options{Client: client, Applications: map[core.Target]Application{blogTarget: {Namespace: "argocd"}}}, wantErr: "namespace and a name"},
		{
			name:    "negative timeout",
			opts:    Options{Client: client, Applications: map[core.Target]Application{blogTarget: {Namespace: "argocd", Name: "blog", SyncTimeout: -time.Second}}},
			wantErr: "must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, err := New(tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("New error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			app, ok := w.Application(blogTarget)
			if !ok || app.SyncTimeout != core.DefaultSyncTimeout || w.resync != DefaultResyncInterval {
				t.Fatalf("defaults = %+v, %s", app, w.resync)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt  int
		min, max time.Duration
	}{
		{attempt: 1, min: 500 * time.Millisecond, max: time.Second},
		{attempt: 2, min: time.Second, max: 2 * time.Second},
		{attempt: 6, min: 15 * time.Second, max: 30 * time.Second},
		{attempt: 100, min: 15 * time.Second, max: 30 * time.Second},
	}
	for _, tt := range tests {
		got := backoff(tt.attempt)
		if got < tt.min || got > tt.max {
			t.Errorf("backoff(%d) = %s, want between %s and %s", tt.attempt, got, tt.min, tt.max)
		}
	}
}
