package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	Actor = "doorstep"

	DefaultBeforePollInterval   = 30 * time.Second
	DefaultVerifyPollInterval   = 10 * time.Second
	DefaultResolveRetryInterval = 15 * time.Second
	DefaultResolveTimeout       = 15 * time.Minute
	DefaultPinTimeout           = 45 * time.Minute
	DefaultSyncGrace            = 10 * time.Second
	DefaultCleanupTimeout       = 15 * time.Second
)

type Options struct {
	Store       DeploymentStore
	Resolver    DigestResolver
	Pins        PinWriter
	Sync        SyncWatcher
	Gates       GateEvaluator
	Blocks      BlockList
	Notifier    Notifier
	Metrics     Metrics
	Policy      FailurePolicy
	SyncTimeout func(core.Target) (time.Duration, bool)
	Logger      *slog.Logger
	Now         func() time.Time

	BeforePollInterval   time.Duration
	VerifyPollInterval   time.Duration
	ResolveRetryInterval time.Duration
	ResolveTimeout       time.Duration
	PinTimeout           time.Duration
	SyncGrace            time.Duration
	CleanupTimeout       time.Duration
}

type Engine struct {
	store       DeploymentStore
	resolver    DigestResolver
	pins        PinWriter
	sync        SyncWatcher
	gates       GateEvaluator
	blocks      BlockList
	notifier    Notifier
	metrics     Metrics
	policy      FailurePolicy
	syncTimeout func(core.Target) (time.Duration, bool)
	logger      *slog.Logger
	now         func() time.Time

	beforePoll     time.Duration
	verifyPoll     time.Duration
	resolveRetry   time.Duration
	resolveTimeout time.Duration
	pinTimeout     time.Duration
	syncGrace      time.Duration
	cleanupTimeout time.Duration
}

type step func(ctx context.Context, d core.Deployment) (core.Deployment, Result, error)

func New(opts Options) (*Engine, error) {
	var errs []error
	required := []struct {
		missing bool
		name    string
	}{
		{opts.Store == nil, "a deployment store"},
		{opts.Resolver == nil, "a digest resolver"},
		{opts.Pins == nil, "a pin writer"},
		{opts.Sync == nil, "a sync watcher"},
		{opts.Gates == nil, "a gate evaluator"},
		{opts.Blocks == nil, "a block list"},
		{opts.SyncTimeout == nil, "the sync timeout of each app"},
	}
	for _, r := range required {
		if r.missing {
			errs = append(errs, errors.New("engine: "+r.name+" is required"))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	e := &Engine{
		store:          opts.Store,
		resolver:       opts.Resolver,
		pins:           opts.Pins,
		sync:           opts.Sync,
		gates:          opts.Gates,
		blocks:         opts.Blocks,
		notifier:       opts.Notifier,
		metrics:        opts.Metrics,
		policy:         opts.Policy,
		syncTimeout:    opts.SyncTimeout,
		logger:         opts.Logger,
		now:            opts.Now,
		beforePoll:     orDefault(opts.BeforePollInterval, DefaultBeforePollInterval),
		verifyPoll:     orDefault(opts.VerifyPollInterval, DefaultVerifyPollInterval),
		resolveRetry:   orDefault(opts.ResolveRetryInterval, DefaultResolveRetryInterval),
		resolveTimeout: orDefault(opts.ResolveTimeout, DefaultResolveTimeout),
		pinTimeout:     orDefault(opts.PinTimeout, DefaultPinTimeout),
		syncGrace:      orDefault(opts.SyncGrace, DefaultSyncGrace),
		cleanupTimeout: orDefault(opts.CleanupTimeout, DefaultCleanupTimeout),
	}
	if e.notifier == nil {
		e.notifier = nopNotifier{}
	}
	if e.metrics == nil {
		e.metrics = nopMetrics{}
	}
	if e.policy == nil {
		e.policy = FailPolicy{}
	}
	if e.logger == nil {
		e.logger = slog.New(slog.DiscardHandler)
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e, nil
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

func (e *Engine) Handle(ctx context.Context, c Claim) (Result, error) {
	for _, id := range c.Superseded {
		e.announceSuperseded(ctx, id)
	}
	d := c.Deployment
	for !d.State.Terminal() {
		run, ok := e.stepFor(d.State)
		if !ok {
			return Result{}, errors.New("engine: no step for state " + string(d.State))
		}
		next, result, err := run(ctx, d)
		if err != nil || ctx.Err() != nil {
			return e.interrupted(ctx, d, err)
		}
		if result.RequeueAfter > 0 {
			return result, nil
		}
		d = next
	}
	return Result{}, nil
}

func (e *Engine) stepFor(state core.State) (step, bool) {
	switch state {
	case core.StateQueued:
		return e.admit, true
	case core.StateResolving:
		return e.resolve, true
	case core.StatePinning:
		return e.pin, true
	case core.StateSyncing:
		return e.syncStep, true
	case core.StateVerifying:
		return e.verify, true
	case core.StateSucceeded, core.StateFailed, core.StateCancelled, core.StateSuperseded:
		return nil, false
	default:
		return nil, false
	}
}

func (e *Engine) interrupted(ctx context.Context, d core.Deployment, err error) (Result, error) {
	conflict := errors.Is(err, core.ErrStateConflict)
	leaseLost := errors.Is(context.Cause(ctx), core.ErrLeaseLost)
	if d.State == core.StatePinning && (conflict || leaseLost) {
		e.abandonIfStopped(ctx, d.ID)
	}
	switch {
	case ctx.Err() != nil:
		return Result{}, ctx.Err()
	case conflict:
		e.logFor(d).InfoContext(ctx, "deployment changed by someone else; stopping", slog.String("error", err.Error()))
		return Result{}, nil
	default:
		return Result{}, err
	}
}

func (e *Engine) abandonIfStopped(ctx context.Context, id core.DeploymentID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cleanupTimeout)
	defer cancel()
	d, err := e.store.Get(ctx, id)
	if err != nil {
		e.logger.WarnContext(ctx, "reload interrupted deployment", slog.Int64("deployment_id", int64(id)), slog.String("error", err.Error()))
		return
	}
	if d.State == core.StateCancelled || d.State == core.StateSuperseded {
		e.abandon(ctx, d)
	}
}

func (e *Engine) abandon(ctx context.Context, d core.Deployment) {
	if d.Digest == "" {
		return
	}
	if err := e.pins.Abandon(ctx, PinRequest{Deployment: d, Digest: d.Digest}); err != nil {
		e.logFor(d).WarnContext(ctx, "abandon pin", slog.String("error", err.Error()))
	}
}

func (e *Engine) transition(ctx context.Context, d core.Deployment, to core.State, message string, lineage core.Lineage) (core.Deployment, error) {
	return e.store.Transition(ctx, Transition{ID: d.ID, From: d.State, To: to, Actor: Actor, Message: message, Lineage: lineage})
}

func (e *Engine) elapsed(ctx context.Context, d core.Deployment) (time.Duration, error) {
	entered, err := e.store.StateEnteredAt(ctx, d.ID, d.State)
	if err != nil {
		return 0, err
	}
	return e.now().Sub(entered), nil
}

func (e *Engine) logFor(d core.Deployment) *slog.Logger {
	return e.logger.With(
		slog.Int64("deployment_id", int64(d.ID)),
		slog.String("target", d.Target.String()),
		slog.String("kind", string(d.Kind)),
		slog.String("state", string(d.State)),
	)
}

func runDuration(d core.Deployment) time.Duration {
	if d.StartedAt == nil || d.FinishedAt == nil {
		return 0
	}
	return max(d.FinishedAt.Sub(*d.StartedAt), 0)
}
