package deploys

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type Repository interface {
	LockTarget(ctx context.Context, t core.Target) error
	Blocked(ctx context.Context, t core.Target, commit core.Commit) (core.Block, bool, error)
	EnqueueDeployment(ctx context.Context, d core.Deployment, priority core.Priority) (core.Deployment, bool, error)
	GetDeployment(ctx context.Context, id core.DeploymentID) (core.Deployment, error)
	TransitionDeployment(ctx context.Context, t store.Transition) (core.Deployment, error)
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
