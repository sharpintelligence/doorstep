package queue

import (
	"context"
	"log/slog"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const DefaultLease = 60 * time.Second

type Options struct {
	Store    *store.Store
	Lease    time.Duration
	MaxBatch func(app string) int
	Logger   *slog.Logger
}

type Queue struct {
	store    *store.Store
	lease    time.Duration
	maxBatch func(app string) int
	logger   *slog.Logger
}

type EnqueueOptions struct {
	Urgent bool
}

type EnqueueResult struct {
	Deployment core.Deployment
	Created    bool
}

type Claimed struct {
	Deployment     core.Deployment
	Attempt        int
	LeaseExpiresAt time.Time
	Superseded     []core.DeploymentID
}

func New(opts Options) *Queue {
	q := &Queue{
		store:    opts.Store,
		lease:    opts.Lease,
		maxBatch: opts.MaxBatch,
		logger:   opts.Logger,
	}
	if q.lease <= 0 {
		q.lease = DefaultLease
	}
	if q.maxBatch == nil {
		q.maxBatch = func(string) int { return 1 }
	}
	if q.logger == nil {
		q.logger = slog.New(slog.DiscardHandler)
	}
	return q
}

func PriorityFor(kind core.Kind, urgent bool) core.Priority {
	switch {
	case kind == core.KindRollback:
		return core.PriorityRollback
	case urgent:
		return core.PriorityUrgent
	default:
		return core.PriorityNormal
	}
}

func (q *Queue) Lease() time.Duration {
	return q.lease
}

func (q *Queue) HeartbeatInterval() time.Duration {
	return q.lease / 3
}

func (q *Queue) Enqueue(ctx context.Context, d core.Deployment, opts EnqueueOptions) (EnqueueResult, error) {
	got, created, err := q.store.EnqueueDeployment(ctx, d, PriorityFor(d.Kind, opts.Urgent))
	if err != nil {
		return EnqueueResult{}, err
	}
	return EnqueueResult{Deployment: got, Created: created}, nil
}

func (q *Queue) Expedite(ctx context.Context, id core.DeploymentID, actor string) error {
	return q.store.ExpediteDeployment(ctx, id, core.PriorityUrgent, actor)
}

func (q *Queue) Claim(ctx context.Context, workerID string) (Claimed, bool, error) {
	got, ok, err := q.store.ClaimBatch(ctx, workerID, q.lease, func(t core.Target) int {
		return q.maxBatch(t.App)
	})
	if err != nil || !ok {
		return Claimed{}, false, err
	}
	if len(got.Superseded) > 0 {
		q.logger.InfoContext(ctx, "deployments superseded",
			slog.Int64("deployment_id", int64(got.Deployment.ID)),
			slog.String("target", got.Deployment.Target.String()),
			slog.Any("superseded", got.Superseded),
		)
	}
	return Claimed{
		Deployment:     got.Deployment,
		Attempt:        got.Claim.Attempt,
		LeaseExpiresAt: got.Claim.LeaseExpiresAt,
		Superseded:     got.Superseded,
	}, true, nil
}

func (q *Queue) Heartbeat(ctx context.Context, id core.DeploymentID, workerID string) (time.Time, error) {
	return q.store.HeartbeatQueueEntry(ctx, id, workerID, q.lease)
}

func (q *Queue) Release(ctx context.Context, id core.DeploymentID, workerID string, delay time.Duration) error {
	return q.store.ReleaseQueueEntry(ctx, id, workerID, max(delay, 0))
}

func (q *Queue) Complete(ctx context.Context, id core.DeploymentID, workerID string) error {
	return q.store.CompleteQueueEntry(ctx, id, workerID)
}

func (q *Queue) Entries(ctx context.Context, app string) ([]core.QueueEntry, error) {
	return q.store.QueueEntries(ctx, app)
}

func (q *Queue) Depth(ctx context.Context) (int, error) {
	return q.store.QueueDepth(ctx)
}

func (q *Queue) Listen(ctx context.Context, wake func()) error {
	return q.store.ListenQueue(ctx, wake, q.logger)
}
