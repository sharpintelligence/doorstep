package intake

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
)

type Queue interface {
	Enqueue(ctx context.Context, d core.Deployment) (core.Deployment, error)
}

type Blocks interface {
	Blocked(ctx context.Context, t core.Target, commit core.Commit) (core.Block, bool, error)
}

type EnqueueFunc func(ctx context.Context, q Queue, b Blocks) (core.DeploymentID, error)

type HeadCheck func(ctx context.Context) (bool, error)

type Commits interface {
	Track(ctx context.Context, c core.TrackedCommit, isHead HeadCheck) (core.TrackedCommit, core.TrackOutcome, error)
	Tracked(ctx context.Context, repository, sha string) ([]core.TrackedCommit, error)
	Settle(ctx context.Context, id core.TrackedCommitID, eval core.ChecksEvaluation, enqueue EnqueueFunc) (core.TrackedCommit, bool, error)
	Open(ctx context.Context, followed []core.FollowedBranch, limit int) ([]core.TrackedCommit, error)
}

type CheckSource interface {
	Checks(ctx context.Context, repository, sha string, names []string) (Snapshot, error)
}

type Branches interface {
	BranchHead(ctx context.Context, repository, branch string) (string, error)
}

type MigrationSource interface {
	Range(ctx context.Context, repository, base, head string, limit int) (github.CommitRange, error)
	Commit(ctx context.Context, repository, sha string) (github.CommitInfo, error)
	PullRequests(ctx context.Context, repository, sha string) ([]github.CommitPullRequest, error)
}

type Baselines interface {
	LastDeployCommit(ctx context.Context, t core.Target) (core.Commit, bool, error)
}
