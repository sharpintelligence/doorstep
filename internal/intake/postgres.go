package intake

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type PostgresCommits struct {
	store    *store.Store
	queueFor func(tx *store.Store) Queue
}

func NewPostgresCommits(st *store.Store, queueFor func(tx *store.Store) Queue) *PostgresCommits {
	return &PostgresCommits{store: st, queueFor: queueFor}
}

func (p *PostgresCommits) Track(ctx context.Context, c core.TrackedCommit, isHead HeadCheck) (core.TrackedCommit, core.TrackOutcome, error) {
	return p.store.TrackCommit(ctx, c, isHead)
}

func (p *PostgresCommits) Tracked(ctx context.Context, repository, sha string) ([]core.TrackedCommit, error) {
	return p.store.TrackedCommits(ctx, repository, sha)
}

func (p *PostgresCommits) Settle(ctx context.Context, id core.TrackedCommitID, eval core.ChecksEvaluation, enqueue EnqueueFunc) (core.TrackedCommit, bool, error) {
	return p.store.SettleTrackedCommit(ctx, id, eval, func(tx *store.Store) (core.DeploymentID, error) {
		return enqueue(ctx, p.queueFor(tx), tx)
	})
}

func (p *PostgresCommits) Open(ctx context.Context, followed []core.FollowedBranch, limit int) ([]core.TrackedCommit, error) {
	return p.store.OpenTrackedCommits(ctx, followed, limit)
}
