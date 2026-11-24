package locks

import (
	"context"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type Repository interface {
	AcquireFreeze(ctx context.Context, req core.LockRequest) (core.Lock, []core.Lock, error)
	ReleaseFreeze(ctx context.Context, target *core.Target, actor string) (core.Lock, error)
	ListFreezes(ctx context.Context) ([]core.Lock, error)
	BlockingFreezes(ctx context.Context, t core.Target) ([]core.Lock, error)
	RecordAudit(ctx context.Context, e core.AuditEntry) error

	InTx(ctx context.Context, fn func(Repository) error) error
}

type Sweeper interface {
	DeleteExpiredFreezes(ctx context.Context) ([]core.Lock, error)
	ClaimLockReminders(ctx context.Context, every time.Duration) ([]core.Lock, error)
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
