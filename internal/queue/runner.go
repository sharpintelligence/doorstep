package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	DefaultConcurrency  = 4
	DefaultPollInterval = 5 * time.Second

	releaseTimeout = 5 * time.Second
	minBackoff     = 5 * time.Second
	maxBackoff     = 5 * time.Minute
)

type Source interface {
	Claim(ctx context.Context, workerID string) (Claimed, bool, error)
	Heartbeat(ctx context.Context, id core.DeploymentID, workerID string) (time.Time, error)
	Release(ctx context.Context, id core.DeploymentID, workerID string, delay time.Duration) error
}

type Result struct {
	RequeueAfter time.Duration
}

type Handler func(ctx context.Context, c Claimed) (Result, error)

type RunnerOptions struct {
	Source            Source
	Listen            func(ctx context.Context, wake func()) error
	WorkerID          string
	Concurrency       int
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	Backoff           func(failures int) time.Duration
	Logger            *slog.Logger
}

type Runner struct {
	source            Source
	listen            func(ctx context.Context, wake func()) error
	workerID          string
	concurrency       int
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	backoff           func(failures int) time.Duration
	logger            *slog.Logger

	mu       sync.Mutex
	failures map[core.DeploymentID]int
}

func NewRunner(opts RunnerOptions) *Runner {
	r := &Runner{
		source:            opts.Source,
		listen:            opts.Listen,
		workerID:          opts.WorkerID,
		concurrency:       opts.Concurrency,
		pollInterval:      opts.PollInterval,
		heartbeatInterval: opts.HeartbeatInterval,
		backoff:           opts.Backoff,
		logger:            opts.Logger,
		failures:          map[core.DeploymentID]int{},
	}
	if r.concurrency <= 0 {
		r.concurrency = DefaultConcurrency
	}
	if r.pollInterval <= 0 {
		r.pollInterval = DefaultPollInterval
	}
	if r.heartbeatInterval <= 0 {
		r.heartbeatInterval = DefaultLease / 3
	}
	if r.backoff == nil {
		r.backoff = ExponentialBackoff
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}
	return r
}

func ExponentialBackoff(failures int) time.Duration {
	delay := minBackoff
	for i := 1; i < failures && delay < maxBackoff; i++ {
		delay *= 2
	}
	return min(delay, maxBackoff)
}

func (r *Runner) Run(ctx context.Context, handle Handler) error {
	if r.source == nil {
		return errors.New("queue runner: source is required")
	}
	if strings.TrimSpace(r.workerID) == "" {
		return errors.New("queue runner: worker ID is required")
	}
	if handle == nil {
		return errors.New("queue runner: handler is required")
	}

	wakeups := make(chan struct{}, 1)
	wake := func() {
		select {
		case wakeups <- struct{}{}:
		default:
		}
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	if r.listen != nil {
		wg.Go(func() {
			if err := r.listen(ctx, wake); err != nil && ctx.Err() == nil {
				r.logger.ErrorContext(ctx, "queue listener stopped; relying on polling", slog.String("error", err.Error()))
			}
		})
	}

	slots := make(chan struct{}, r.concurrency)
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		r.claimAvailable(ctx, &wg, slots, handle, wake)
		select {
		case <-ctx.Done():
			return nil
		case <-wakeups:
		case <-ticker.C:
		}
	}
}

func (r *Runner) claimAvailable(ctx context.Context, wg *sync.WaitGroup, slots chan struct{}, handle Handler, wake func()) {
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		c, ok, err := r.source.Claim(ctx, r.workerID)
		if err != nil || !ok {
			<-slots
			if err != nil && ctx.Err() == nil {
				r.logger.ErrorContext(ctx, "claim queue entry", slog.String("error", err.Error()))
			}
			return
		}
		wg.Go(func() {
			defer func() {
				<-slots
				wake()
			}()
			r.process(ctx, handle, c)
		})
	}
}

func (r *Runner) process(ctx context.Context, handle Handler, c Claimed) {
	id := c.Deployment.ID
	logger := r.logger.With(
		slog.Int64("deployment_id", int64(id)),
		slog.String("target", c.Deployment.Target.String()),
		slog.Int("attempt", c.Attempt),
	)
	handlerCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var heartbeats sync.WaitGroup
	heartbeats.Go(func() { r.heartbeat(handlerCtx, cancel, id, logger) })
	result, err := r.call(handlerCtx, handle, c)
	cancel(nil)
	heartbeats.Wait()

	if errors.Is(context.Cause(handlerCtx), core.ErrLeaseLost) {
		logger.WarnContext(ctx, "queue lease lost; handler cancelled")
		r.forget(id)
		return
	}

	delay := max(result.RequeueAfter, 0)
	switch {
	case ctx.Err() != nil:
		delay = 0
	case err != nil:
		delay = r.backoff(r.recordFailure(id))
		logger.ErrorContext(ctx, "queue handler failed", slog.String("error", err.Error()), slog.Duration("retry_in", delay))
	default:
		r.forget(id)
	}

	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancelRelease()
	switch err := r.source.Release(releaseCtx, id, r.workerID, delay); {
	case errors.Is(err, core.ErrLeaseLost):
		r.forget(id)
	case err != nil:
		logger.ErrorContext(ctx, "release queue entry", slog.String("error", err.Error()))
	}
}

func (r *Runner) call(ctx context.Context, handle Handler, c Claimed) (result Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("queue handler panicked: %v", p)
		}
	}()
	return handle(ctx, c)
}

func (r *Runner) heartbeat(ctx context.Context, cancel context.CancelCauseFunc, id core.DeploymentID, logger *slog.Logger) {
	ticker := time.NewTicker(r.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_, err := r.source.Heartbeat(ctx, id, r.workerID)
		switch {
		case errors.Is(err, core.ErrLeaseLost):
			cancel(core.ErrLeaseLost)
			return
		case err != nil && ctx.Err() == nil:
			logger.WarnContext(ctx, "queue heartbeat failed", slog.String("error", err.Error()))
		}
	}
}

func (r *Runner) recordFailure(id core.DeploymentID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[id]++
	return r.failures[id]
}

func (r *Runner) forget(id core.DeploymentID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.failures, id)
}
