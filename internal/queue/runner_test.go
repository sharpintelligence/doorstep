package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type fakeItem struct {
	claimed     Claimed
	availableAt time.Time
	leased      bool
	done        bool
}

type releaseCall struct {
	id     core.DeploymentID
	worker string
	delay  time.Duration
	ctxErr error
}

type fakeSource struct {
	lease        time.Duration
	mu           sync.Mutex
	items        []*fakeItem
	claimErr     error
	heartbeatErr error
	claims       int
	heartbeats   int
	releases     []releaseCall
}

func (f *fakeSource) add(id core.DeploymentID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, &fakeItem{claimed: Claimed{Deployment: core.Deployment{
		ID:     id,
		Target: core.Target{App: fmt.Sprintf("app-%d", id), Environment: "prod"},
	}}})
}

func (f *fakeSource) item(id core.DeploymentID) *fakeItem {
	for _, it := range f.items {
		if it.claimed.Deployment.ID == id {
			return it
		}
	}
	return nil
}

func (f *fakeSource) complete(id core.DeploymentID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.item(id).done = true
}

func (f *fakeSource) Lease() time.Duration {
	if f.lease > 0 {
		return f.lease
	}
	return DefaultLease
}

func (f *fakeSource) Claim(context.Context, string) (Claimed, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claimErr != nil {
		return Claimed{}, false, f.claimErr
	}
	now := time.Now()
	for _, it := range f.items {
		if it.leased || it.done || it.availableAt.After(now) {
			continue
		}
		it.leased = true
		it.claimed.Attempt++
		return it.claimed, true, nil
	}
	return Claimed{}, false, nil
}

func (f *fakeSource) Heartbeat(_ context.Context, id core.DeploymentID, _ string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats++
	if f.heartbeatErr != nil {
		return time.Time{}, f.heartbeatErr
	}
	if it := f.item(id); it == nil || it.done || !it.leased {
		return time.Time{}, core.ErrLeaseLost
	}
	return time.Now().Add(f.Lease()), nil
}

func (f *fakeSource) Release(ctx context.Context, id core.DeploymentID, worker string, delay time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, releaseCall{id: id, worker: worker, delay: delay, ctxErr: ctx.Err()})
	it := f.item(id)
	if it == nil || it.done || !it.leased {
		return core.ErrLeaseLost
	}
	it.leased = false
	it.availableAt = time.Now().Add(delay)
	return nil
}

func (f *fakeSource) snapshot() (claims, heartbeats int, releases []releaseCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, f.heartbeats, append([]releaseCall(nil), f.releases...)
}

type runHarness struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan error

	mu         sync.Mutex
	listenWake func()
}

func (h *runHarness) wake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listenWake()
}

func startRunner(t *testing.T, opts RunnerOptions, handle Handler) *runHarness {
	t.Helper()
	h := &runHarness{t: t, done: make(chan error, 1)}
	opts.Listen = func(ctx context.Context, wake func()) error {
		h.mu.Lock()
		h.listenWake = wake
		h.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	if opts.WorkerID == "" {
		opts.WorkerID = "worker-1"
	}
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel
	r := NewRunner(opts)
	go func() { h.done <- r.Run(ctx, handle) }()
	synctest.Wait()
	return h
}

func (h *runHarness) stop() {
	h.t.Helper()
	h.cancel()
	if err := <-h.done; err != nil {
		h.t.Errorf("Run = %v, want nil after cancellation", err)
	}
}

func TestRunnerHandlesEveryClaimAndReleases(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{}
		for id := range core.DeploymentID(3) {
			src.add(id + 1)
		}
		var mu sync.Mutex
		handled := map[core.DeploymentID]int{}
		h := startRunner(t, RunnerOptions{Source: src}, func(_ context.Context, c Claimed) (Result, error) {
			mu.Lock()
			defer mu.Unlock()
			handled[c.Deployment.ID]++
			if c.Deployment.ID == 2 && handled[2] == 1 {
				return Result{RequeueAfter: time.Minute}, nil
			}
			src.complete(c.Deployment.ID)
			return Result{}, nil
		})

		mu.Lock()
		if len(handled) != 3 || handled[2] != 1 {
			t.Errorf("handled %v, want each of 3 deployments once", handled)
		}
		mu.Unlock()

		time.Sleep(time.Minute)
		synctest.Wait()
		mu.Lock()
		if handled[2] != 2 {
			t.Errorf("deployment 2 handled %d times after its requeue delay, want 2", handled[2])
		}
		mu.Unlock()

		h.stop()
		_, _, releases := src.snapshot()
		var requeued bool
		for _, rel := range releases {
			if rel.id == 2 && rel.delay == time.Minute {
				requeued = true
			}
			if rel.worker != "worker-1" {
				t.Errorf("released by %q, want worker-1", rel.worker)
			}
		}
		if !requeued || len(releases) != 4 {
			t.Errorf("releases = %+v, want 4 including a one minute requeue of deployment 2", releases)
		}
	})
}

func TestRunnerWakeAndPoll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		wake     bool
		wait     time.Duration
		wantDone bool
	}{
		{name: "wake claims at once", wake: true, wantDone: true},
		{name: "without wake nothing happens before the poll", wait: DefaultPollInterval - time.Millisecond},
		{name: "poll picks up work", wait: DefaultPollInterval, wantDone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				src := &fakeSource{}
				var handled atomic.Int32
				h := startRunner(t, RunnerOptions{Source: src}, func(_ context.Context, c Claimed) (Result, error) {
					handled.Add(1)
					src.complete(c.Deployment.ID)
					return Result{}, nil
				})
				src.add(1)
				if tt.wake {
					h.wake()
				}
				time.Sleep(tt.wait)
				synctest.Wait()
				if got := handled.Load() == 1; got != tt.wantDone {
					t.Errorf("handled = %v, want %v", got, tt.wantDone)
				}
				h.stop()
			})
		})
	}
}

func TestRunnerConcurrencyCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		concurrency int
		want        int32
	}{
		{name: "default", concurrency: 0, want: DefaultConcurrency},
		{name: "one", concurrency: 1, want: 1},
		{name: "three", concurrency: 3, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				src := &fakeSource{}
				for id := range core.DeploymentID(10) {
					src.add(id + 1)
				}
				var running, peak, total atomic.Int32
				h := startRunner(t, RunnerOptions{Source: src, Concurrency: tt.concurrency}, func(_ context.Context, c Claimed) (Result, error) {
					n := running.Add(1)
					for {
						p := peak.Load()
						if n <= p || peak.CompareAndSwap(p, n) {
							break
						}
					}
					time.Sleep(time.Second)
					running.Add(-1)
					total.Add(1)
					src.complete(c.Deployment.ID)
					return Result{}, nil
				})
				if got := running.Load(); got != tt.want {
					t.Errorf("running = %d, want %d", got, tt.want)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if peak.Load() != tt.want || total.Load() != 10 {
					t.Errorf("peak %d, total %d; want peak %d and all 10 handled", peak.Load(), total.Load(), tt.want)
				}
				h.stop()
			})
		})
	}
}

func TestRunnerHeartbeats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		lease    time.Duration
		interval time.Duration
		work     time.Duration
		want     int
	}{
		{name: "default interval", work: 70 * time.Second, want: 3},
		{name: "default interval from a short lease", lease: 9 * time.Second, work: 10 * time.Second, want: 3},
		{name: "custom interval", interval: 10 * time.Second, work: 35 * time.Second, want: 3},
		{name: "short work", interval: 10 * time.Second, work: 5 * time.Second, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				src := &fakeSource{lease: tt.lease}
				src.add(1)
				h := startRunner(t, RunnerOptions{Source: src, HeartbeatInterval: tt.interval}, func(_ context.Context, c Claimed) (Result, error) {
					time.Sleep(tt.work)
					src.complete(c.Deployment.ID)
					return Result{}, nil
				})
				time.Sleep(tt.work + time.Second)
				synctest.Wait()
				if _, heartbeats, _ := src.snapshot(); heartbeats != tt.want {
					t.Errorf("%d heartbeats, want %d", heartbeats, tt.want)
				}
				h.stop()
			})
		})
	}
}

func TestRunnerLeaseLossCancelsHandler(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{heartbeatErr: fmt.Errorf("heartbeat: %w", core.ErrLeaseLost)}
		src.add(1)
		var cause error
		h := startRunner(t, RunnerOptions{Source: src, HeartbeatInterval: time.Second}, func(ctx context.Context, _ Claimed) (Result, error) {
			<-ctx.Done()
			cause = context.Cause(ctx)
			return Result{}, ctx.Err()
		})
		time.Sleep(time.Second)
		synctest.Wait()
		if !errors.Is(cause, core.ErrLeaseLost) {
			t.Errorf("handler context cause = %v, want ErrLeaseLost", cause)
		}
		h.stop()
		if _, _, releases := src.snapshot(); len(releases) != 0 {
			t.Errorf("released %+v after losing the lease", releases)
		}
	})
}

func TestRunnerTransientHeartbeatErrorKeepsHandler(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{heartbeatErr: errors.New("connection reset")}
		src.add(1)
		var finished atomic.Bool
		h := startRunner(t, RunnerOptions{Source: src, HeartbeatInterval: time.Second}, func(ctx context.Context, c Claimed) (Result, error) {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(5 * time.Second):
			}
			finished.Store(true)
			src.complete(c.Deployment.ID)
			return Result{}, nil
		})
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if !finished.Load() {
			t.Error("handler was cancelled by a transient heartbeat error")
		}
		h.stop()
	})
}

func TestRunnerBackoff(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{}
		src.add(1)
		var calls atomic.Int32
		backoff := func(failures int) time.Duration { return time.Duration(failures) * time.Minute }
		h := startRunner(t, RunnerOptions{Source: src, Backoff: backoff}, func(context.Context, Claimed) (Result, error) {
			switch calls.Add(1) {
			case 1, 2:
				return Result{RequeueAfter: time.Hour}, errors.New("registry unavailable")
			case 3:
				return Result{}, nil
			default:
				return Result{}, errors.New("failed again")
			}
		})
		time.Sleep(time.Minute)
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		h.stop()

		_, _, releases := src.snapshot()
		want := []time.Duration{time.Minute, 2 * time.Minute, 0, time.Minute}
		if len(releases) != len(want) {
			t.Fatalf("releases = %+v, want delays %v", releases, want)
		}
		for i, rel := range releases {
			if rel.delay != want[i] {
				t.Errorf("release %d delay = %s, want %s", i, rel.delay, want[i])
			}
		}
	})
}

func TestRunnerForgetsStaleFailures(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := NewRunner(RunnerOptions{Source: &fakeSource{}})
		if got := r.recordFailure(1); got != 1 {
			t.Fatalf("first failure of 1 = %d", got)
		}
		if got := r.recordFailure(1); got != 2 {
			t.Fatalf("second failure of 1 = %d", got)
		}
		time.Sleep(failureMemory + time.Second)
		if got := r.recordFailure(2); got != 1 {
			t.Fatalf("first failure of 2 = %d", got)
		}
		if _, ok := r.failures[1]; ok {
			t.Errorf("failures still tracks deployment 1 after %s: %v", failureMemory, r.failures)
		}
		if got := r.recordFailure(1); got != 1 {
			t.Errorf("failure of 1 after it was forgotten = %d, want 1", got)
		}
		if len(r.failures) != 2 {
			t.Errorf("failures = %v, want two entries", r.failures)
		}
	})
}

func TestRunnerHandlerPanicIsAFailure(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{}
		src.add(1)
		h := startRunner(t, RunnerOptions{Source: src}, func(context.Context, Claimed) (Result, error) {
			panic("boom")
		})
		h.stop()
		_, _, releases := src.snapshot()
		if len(releases) != 1 || releases[0].delay != ExponentialBackoff(1) {
			t.Errorf("releases = %+v, want one release with the first backoff", releases)
		}
	})
}

func TestRunnerShutdownReleasesInFlightWork(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{}
		src.add(1)
		h := startRunner(t, RunnerOptions{Source: src}, func(ctx context.Context, _ Claimed) (Result, error) {
			<-ctx.Done()
			return Result{RequeueAfter: time.Hour}, ctx.Err()
		})
		h.stop()
		_, _, releases := src.snapshot()
		if len(releases) != 1 {
			t.Fatalf("releases = %+v, want one", releases)
		}
		if releases[0].delay != 0 || releases[0].ctxErr != nil {
			t.Errorf("shutdown release = %+v, want no delay on a live context", releases[0])
		}
	})
}

func TestRunnerClaimErrorWaitsForPoll(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{claimErr: errors.New("database down")}
		h := startRunner(t, RunnerOptions{Source: src, PollInterval: time.Second}, func(context.Context, Claimed) (Result, error) {
			return Result{}, nil
		})
		time.Sleep(10*time.Second + time.Millisecond)
		synctest.Wait()
		if claims, _, _ := src.snapshot(); claims != 11 {
			t.Errorf("%d claims in 10 poll intervals, want 11", claims)
		}
		h.stop()
	})
}

func TestRunnerListenFailureFallsBackToPolling(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		src := &fakeSource{}
		var handled atomic.Int32
		r := NewRunner(RunnerOptions{
			Source:   src,
			WorkerID: "w1",
			Listen:   func(context.Context, func()) error { return errors.New("listen refused") },
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- r.Run(ctx, func(_ context.Context, c Claimed) (Result, error) {
				handled.Add(1)
				src.complete(c.Deployment.ID)
				return Result{}, nil
			})
		}()
		synctest.Wait()
		src.add(1)
		time.Sleep(DefaultPollInterval)
		synctest.Wait()
		if handled.Load() != 1 {
			t.Errorf("handled %d, want 1 through polling", handled.Load())
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
}

func TestRunnerRejectsInvalidOptions(t *testing.T) {
	t.Parallel()

	handler := func(context.Context, Claimed) (Result, error) { return Result{}, nil }
	tests := []struct {
		name    string
		opts    RunnerOptions
		handler Handler
	}{
		{name: "no source", opts: RunnerOptions{WorkerID: "w1"}, handler: handler},
		{name: "no worker", opts: RunnerOptions{Source: &fakeSource{}, WorkerID: " "}, handler: handler},
		{name: "no handler", opts: RunnerOptions{Source: &fakeSource{}, WorkerID: "w1"}},
		{name: "heartbeat as long as the lease", opts: RunnerOptions{Source: &fakeSource{lease: 10 * time.Second}, WorkerID: "w1", HeartbeatInterval: 10 * time.Second}, handler: handler},
		{name: "heartbeat longer than the lease", opts: RunnerOptions{Source: &fakeSource{}, WorkerID: "w1", HeartbeatInterval: 2 * DefaultLease}, handler: handler},
		{name: "lease too short for a heartbeat", opts: RunnerOptions{Source: &fakeSource{lease: 2 * time.Nanosecond}, WorkerID: "w1"}, handler: handler},
	}
	for _, tt := range tests {
		if err := NewRunner(tt.opts).Run(t.Context(), tt.handler); err == nil {
			t.Errorf("%s: Run succeeded", tt.name)
		}
	}
}

func TestExponentialBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		failures int
		want     time.Duration
	}{
		{0, 5 * time.Second},
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{7, 5 * time.Minute},
		{1000, 5 * time.Minute},
	}
	for _, tt := range tests {
		if got := ExponentialBackoff(tt.failures); got != tt.want {
			t.Errorf("ExponentialBackoff(%d) = %s, want %s", tt.failures, got, tt.want)
		}
	}
}
