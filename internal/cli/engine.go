package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sharpintelligence/doorstep/internal/argocd"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/gitops"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/locks"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/notify"
	"github.com/sharpintelligence/doorstep/internal/queue"
	"github.com/sharpintelligence/doorstep/internal/registry"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const announceTimeout = 15 * time.Second

type engineInput struct {
	config    config.Config
	catalog   config.Catalog
	apps      catalogApps
	store     *store.Store
	queue     *queue.Queue
	githubApp *github.App
	watcher   *argocd.Watcher
	locks     *locks.Service
	blocks    engine.BlockList
	notifier  notify.Notifier
	recorder  *metrics.Recorder
	logger    *slog.Logger
	lookupEnv LookupEnv
}

type engineWiring struct {
	engine *engine.Engine
	runs   []func(context.Context) error
	drains []func(context.Context) error
}

func newEngine(in engineInput) (engineWiring, error) {
	statusMirror, err := newGateStatusMirror(in.catalog, in.config, in.githubApp, in.logger, in.recorder)
	if err != nil {
		return engineWiring{}, err
	}
	evaluator, err := newGateEvaluator(gateDeps{
		statusMirror: statusMirror,
		store:        in.store,
		githubApp:    in.githubApp,
		locks:        in.locks,
		watcher:      in.watcher,
		catalog:      in.catalog,
		config:       in.config,
		recorder:     in.recorder,
		logger:       in.logger,
		lookupEnv:    in.lookupEnv,
	})
	if err != nil {
		return engineWiring{}, err
	}
	links, err := notificationLinks(in.config)
	if err != nil {
		return engineWiring{}, err
	}
	resolverOpts := registry.Options{UserAgent: userAgent()}
	if path := in.config.Registry.CredentialsFile; path != "" {
		resolverOpts.Credentials = registry.FileCredentials{Path: path}
	}
	targets := make(map[core.Target]gitops.Target, len(in.apps))
	for _, a := range in.apps {
		target := a.gitops()
		if err := target.Validate(); err != nil {
			return engineWiring{}, fmt.Errorf("app %s: gitops target: %w", a.app.Name, err)
		}
		targets[a.target()] = target
		in.recorder.PrimeApp(a.app.Name)
	}
	policy, err := newRetryPolicy(in)
	if err != nil {
		return engineWiring{}, err
	}
	e, err := engine.New(engine.Options{
		Store:    engineStore{store: in.store},
		Resolver: digestResolver{resolver: registry.New(resolverOpts), logger: in.logger},
		Pins: pinWriter{
			writer:  gitops.New(in.githubApp, gitops.Options{DeploymentURL: links.DeploymentURL, Logger: in.logger}),
			targets: targets,
		},
		Sync:        in.watcher,
		Gates:       evaluator,
		Blocks:      in.blocks,
		Notifier:    in.notifier,
		Metrics:     engineMetrics{recorder: in.recorder},
		Policy:      policy,
		SyncTimeout: in.apps.SyncTimeout,
		Logger:      in.logger,
	})
	if err != nil {
		return engineWiring{}, err
	}
	workerID, err := queue.NewWorkerID()
	if err != nil {
		return engineWiring{}, err
	}
	runner := queue.NewRunner(queue.RunnerOptions{
		Source:      in.queue,
		Listen:      in.queue.Listen,
		WorkerID:    workerID,
		Concurrency: in.config.Engine.Concurrency,
		Logger:      in.logger,
	})
	handle := queueHandler(e)
	var drains []func(context.Context) error
	if statusMirror != nil {
		drains = append(drains, statusMirror.Close)
	}
	return engineWiring{
		engine: e,
		drains: drains,
		runs:   []func(context.Context) error{func(ctx context.Context) error { return runner.Run(ctx, handle) }},
	}, nil
}

func queueHandler(e *engine.Engine) queue.Handler {
	return func(ctx context.Context, c queue.Claimed) (queue.Result, error) {
		result, err := e.Handle(ctx, engine.Claim{Deployment: c.Deployment, Attempt: c.Attempt, Superseded: c.Superseded})
		return queue.Result{RequeueAfter: result.RequeueAfter}, err
	}
}

type engineStore struct {
	store *store.Store
}

func (s engineStore) Get(ctx context.Context, id core.DeploymentID) (core.Deployment, error) {
	return s.store.GetDeployment(ctx, id)
}

func (s engineStore) Transition(ctx context.Context, t engine.Transition) (core.Deployment, error) {
	return s.store.TransitionDeployment(ctx, store.Transition{
		ID:            t.ID,
		From:          t.From,
		To:            t.To,
		Actor:         t.Actor,
		Message:       t.Message,
		Data:          t.Data,
		FailureReason: t.FailureReason,
		Lineage:       t.Lineage,
	})
}

func (s engineStore) SetLineage(ctx context.Context, id core.DeploymentID, l core.Lineage, actor string) (core.Deployment, error) {
	return s.store.SetLineage(ctx, id, l, actor)
}

func (s engineStore) TimeInState(ctx context.Context, id core.DeploymentID, state core.State) (time.Duration, error) {
	return s.store.TimeInState(ctx, id, state)
}

func (s engineStore) RetryState(ctx context.Context, id core.DeploymentID) (engine.RetryState, error) {
	state, err := s.store.RetryState(ctx, id)
	if err != nil {
		return engine.RetryState{}, err
	}
	return engine.RetryState{Retries: state.Retries, Delay: state.Delay, MaxAttempts: state.MaxAttempts}, nil
}

type commitResolver interface {
	ResolveCommit(ctx context.Context, repository, sha string) (registry.Digest, error)
}

type digestResolver struct {
	resolver commitResolver
	logger   *slog.Logger
}

func (r digestResolver) Resolve(ctx context.Context, ref core.ImageRef) (core.Digest, error) {
	resolved, err := r.resolver.ResolveCommit(ctx, ref.Repository, ref.Tag)
	switch {
	case err == nil:
		digest, err := core.ParseDigest(string(resolved))
		if err != nil {
			return "", fmt.Errorf("%w: registry returned %w", engine.ErrPermanent, err)
		}
		return digest, nil
	case errors.Is(err, registry.ErrInvalidReference):
		return "", fmt.Errorf("%w: %w", engine.ErrPermanent, err)
	case errors.Is(err, registry.ErrDenied):
		r.logger.WarnContext(ctx, "registry denied access; waiting in case the image is not pushed yet, check the registry credentials otherwise",
			slog.String("image", ref.String()), slog.String("error", err.Error()))
		return "", fmt.Errorf("%w: %w", engine.ErrImageNotReady, err)
	case errors.Is(err, registry.ErrNotFound):
		return "", fmt.Errorf("%w: %w", engine.ErrImageNotReady, err)
	default:
		return "", err
	}
}

type gitopsWriter interface {
	Pin(ctx context.Context, req gitops.Request) (gitops.Result, error)
	Abandon(ctx context.Context, req gitops.Request) error
}

type pinWriter struct {
	writer  gitopsWriter
	targets map[core.Target]gitops.Target
}

func (p pinWriter) Pin(ctx context.Context, req engine.PinRequest) (engine.PinResult, error) {
	r, err := p.request(req)
	if err != nil {
		return engine.PinResult{}, err
	}
	res, err := p.writer.Pin(ctx, r)
	if err != nil {
		return engine.PinResult{}, classifyPinError(err)
	}
	return engine.PinResult{Commit: res.Commit, Previous: res.Previous, Changed: res.Changed}, nil
}

func (p pinWriter) Abandon(ctx context.Context, req engine.PinRequest) error {
	r, err := p.request(req)
	if err != nil {
		return err
	}
	return classifyPinError(p.writer.Abandon(ctx, r))
}

func (p pinWriter) request(req engine.PinRequest) (gitops.Request, error) {
	d := req.Deployment
	target, ok := p.targets[d.Target]
	if !ok {
		return gitops.Request{}, fmt.Errorf("%w: no GitOps target is configured for %s", engine.ErrPermanent, d.Target)
	}
	return gitops.Request{
		Target:     target,
		Digest:     req.Digest,
		Deployment: d.ID,
		Kind:       d.Kind,
		AppCommit:  d.Commit,
		RollbackOf: d.RollbackOfID,
		Blocks:     req.Blocks,
		Unblocks:   req.Unblocks,
		Repin:      req.Repin,
	}, nil
}

func classifyPinError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gitops.ErrPullRequestTampered) {
		return fmt.Errorf("%w: %w", engine.ErrNeedsAttention, err)
	}
	permanent := []error{
		gitops.ErrInvalidRequest,
		gitops.ErrPinNotFound,
		gitops.ErrAmbiguousPin,
		gitops.ErrUnsupportedYAML,
		gitops.ErrBranchProtected,
		gitops.ErrPinOverwritten,
		gitops.ErrPullRequestClosed,
		github.ErrFileTooLarge,
		github.ErrNotAFile,
	}
	for _, sentinel := range permanent {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%w: %w", engine.ErrPermanent, err)
		}
	}
	return err
}

type engineMetrics struct {
	recorder *metrics.Recorder
}

func (m engineMetrics) DeploymentStarted(app string) {
	m.recorder.DeploymentStarted(app)
}

func (m engineMetrics) DeploymentFinished(app string, outcome engine.Outcome, duration time.Duration) {
	m.recorder.DeploymentFinished(app, metrics.Outcome(outcome), duration)
}

func (m engineMetrics) DeploymentRetried(app string, class core.FailureClass) {
	m.recorder.DeploymentRetried(app, metrics.FailureClass(class))
}

type announcer interface {
	Queued(ctx context.Context, id core.DeploymentID)
	Cancelled(ctx context.Context, ids ...core.DeploymentID)
}

func announceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), announceTimeout)
}

type announcingCommits struct {
	intake.Commits
	announcer announcer
}

func (c announcingCommits) Settle(ctx context.Context, id core.TrackedCommitID, eval core.ChecksEvaluation, enqueue intake.EnqueueFunc) (core.TrackedCommit, bool, error) {
	settled, enqueued, err := c.Commits.Settle(ctx, id, eval, enqueue)
	if err == nil && enqueued && settled.DeploymentID != nil {
		ctx, cancel := announceContext(ctx)
		defer cancel()
		c.announcer.Queued(ctx, *settled.DeploymentID)
	}
	return settled, enqueued, err
}

type rollbackRequester interface {
	Rollback(ctx context.Context, req rollback.Request) (rollback.Result, error)
}

type announcingRollbacks struct {
	rollbacks rollbackRequester
	announcer announcer
}

func (r announcingRollbacks) Rollback(ctx context.Context, req rollback.Request) (rollback.Result, error) {
	result, err := r.rollbacks.Rollback(ctx, req)
	if err != nil {
		return result, err
	}
	ctx, cancel := announceContext(ctx)
	defer cancel()
	r.announcer.Queued(ctx, result.Rollback.ID)
	r.announcer.Cancelled(ctx, result.Cancelled...)
	return result, nil
}
