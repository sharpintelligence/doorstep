package rollback

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type Repository interface {
	LockTarget(ctx context.Context, t core.Target) error
	CurrentDeployment(ctx context.Context, t core.Target) (core.Deployment, error)
	LastSuccessfulDeployment(ctx context.Context, t core.Target, excludeDigest core.Digest) (core.Deployment, error)
	MigrationsBetween(ctx context.Context, t core.Target, to, current core.Deployment) ([]core.Deployment, error)
	GetDeployment(ctx context.Context, id core.DeploymentID) (core.Deployment, error)
	ListDeployments(ctx context.Context, f store.DeploymentFilter) (store.DeploymentPage, error)
	TransitionDeployment(ctx context.Context, t store.Transition) (core.Deployment, error)
	EnqueueDeployment(ctx context.Context, d core.Deployment, priority core.Priority) (core.Deployment, bool, error)

	CreateBlock(ctx context.Context, b core.Block) (core.Block, bool, error)
	ClearBlocks(ctx context.Context, t core.Target, ref core.BlockRef, actor string) ([]core.Block, error)
	ActiveBlock(ctx context.Context, t core.Target, ref core.BlockRef) (core.Block, bool, error)
	ListBlocks(ctx context.Context, t core.Target, includeCleared bool) ([]core.Block, error)
	PendingBlockTrailers(ctx context.Context, t core.Target) (core.BlockTrailers, error)
	MarkBlocksMirrored(ctx context.Context, t core.Target, trailers core.BlockTrailers, commit core.Commit) error

	RecordAudit(ctx context.Context, e core.AuditEntry) error

	InTx(ctx context.Context, fn func(Repository) error) error
}

func NewRepository(st *store.Store) Repository {
	return storeRepository{st}
}

type storeRepository struct {
	*store.Store
}

func (r storeRepository) InTx(ctx context.Context, fn func(Repository) error) error {
	return r.Store.InTx(ctx, func(tx *store.Store) error {
		return fn(storeRepository{tx})
	})
}
