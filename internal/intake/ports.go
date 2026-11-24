package intake

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Queue interface {
	Enqueue(ctx context.Context, d core.Deployment) (core.Deployment, error)
}

type EnqueueFunc func(ctx context.Context, q Queue) (core.DeploymentID, error)

type Commits interface {
	Track(ctx context.Context, c core.TrackedCommit) (core.TrackedCommit, error)
	Tracked(ctx context.Context, repository, sha string) ([]core.TrackedCommit, error)
	SaveChecks(ctx context.Context, id core.TrackedCommitID, state core.ChecksState, checks []core.Check) (core.TrackedCommit, error)
	Promote(ctx context.Context, id core.TrackedCommitID, enqueue EnqueueFunc) (core.TrackedCommit, bool, error)
	Pending(ctx context.Context, limit int) ([]core.TrackedCommit, error)
}

type CheckSource interface {
	Checks(ctx context.Context, repository, sha string, withSuites bool) (Snapshot, error)
}
