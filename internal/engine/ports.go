package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Claim struct {
	Deployment core.Deployment
	Attempt    int
	Superseded []core.DeploymentID
}

type Result struct {
	RequeueAfter time.Duration
}

type Transition struct {
	ID            core.DeploymentID
	From          core.State
	To            core.State
	Actor         string
	Message       string
	Data          json.RawMessage
	FailureReason string
	Lineage       core.Lineage
}

type PinRequest struct {
	Deployment core.Deployment
	Digest     core.Digest
	Blocks     []core.Digest
	Unblocks   []core.Digest
}

type PinResult struct {
	Commit   core.Commit
	Previous core.Digest
	Changed  bool
}

type DeploymentStore interface {
	Get(ctx context.Context, id core.DeploymentID) (core.Deployment, error)
	Transition(ctx context.Context, t Transition) (core.Deployment, error)
	SetLineage(ctx context.Context, id core.DeploymentID, l core.Lineage, actor string) (core.Deployment, error)
	StateEnteredAt(ctx context.Context, id core.DeploymentID, state core.State) (time.Time, error)
}

type DigestResolver interface {
	Resolve(ctx context.Context, ref core.ImageRef) (core.Digest, error)
}

type PinWriter interface {
	Pin(ctx context.Context, req PinRequest) (PinResult, error)
	Current(ctx context.Context, t core.Target) (core.Digest, error)
	Abandon(ctx context.Context, req PinRequest) error
}

type SyncWatcher interface {
	WaitSynced(ctx context.Context, t core.Target, revision string) (core.SyncStatus, error)
	Health(ctx context.Context, t core.Target) (core.Health, error)
}

type GateEvaluator interface {
	Evaluate(ctx context.Context, d core.Deployment, phase core.GatePhase) (core.GateOutcome, error)
}

type BlockList interface {
	Blocked(ctx context.Context, t core.Target, commit core.Commit, digest core.Digest) (core.Block, bool, error)
	PendingTrailers(ctx context.Context, t core.Target) (core.BlockTrailers, error)
	MarkMirrored(ctx context.Context, t core.Target, trailers core.BlockTrailers, commit core.Commit) error
}

type Notifier interface {
	Notify(ctx context.Context, n core.Notification)
}

type Metrics interface {
	DeploymentStarted(app string)
	DeploymentFinished(app string, outcome Outcome, duration time.Duration)
}

type Outcome string

const (
	OutcomeSucceeded      Outcome = "succeeded"
	OutcomeFailed         Outcome = "failed"
	OutcomeRolledBack     Outcome = "rolled_back"
	OutcomeRollbackFailed Outcome = "rollback_failed"
	OutcomeCancelled      Outcome = "cancelled"
	OutcomeSuperseded     Outcome = "superseded"
)

type nopNotifier struct{}

func (nopNotifier) Notify(context.Context, core.Notification) {}

type nopMetrics struct{}

func (nopMetrics) DeploymentStarted(string) {}

func (nopMetrics) DeploymentFinished(string, Outcome, time.Duration) {}
