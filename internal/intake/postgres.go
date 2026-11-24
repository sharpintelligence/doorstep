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

func (p *PostgresCommits) Track(ctx context.Context, c core.TrackedCommit) (core.TrackedCommit, error) {
	tracked, _, err := p.store.TrackCommit(ctx, c)
	return tracked, err
}

func (p *PostgresCommits) Tracked(ctx context.Context, repository, sha string) ([]core.TrackedCommit, error) {
	return p.store.TrackedCommits(ctx, repository, sha)
}

func (p *PostgresCommits) SaveChecks(ctx context.Context, id core.TrackedCommitID, state core.ChecksState, checks []core.Check) (core.TrackedCommit, error) {
	return p.store.SaveCommitChecks(ctx, id, state, checks)
}

func (p *PostgresCommits) Promote(ctx context.Context, id core.TrackedCommitID, enqueue EnqueueFunc) (core.TrackedCommit, bool, error) {
	return p.store.PromoteTrackedCommit(ctx, id, func(tx *store.Store) (core.DeploymentID, error) {
		return enqueue(ctx, p.queueFor(tx))
	})
}

func (p *PostgresCommits) Pending(ctx context.Context, limit int) ([]core.TrackedCommit, error) {
	return p.store.PendingTrackedCommits(ctx, limit)
}
