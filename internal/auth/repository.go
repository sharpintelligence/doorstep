package auth

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type Repository interface {
	CreateAPIToken(ctx context.Context, t store.NewAPIToken) (store.APIToken, error)
	FindAPITokenByHash(ctx context.Context, hash [32]byte) (store.APIToken, error)
	TouchAPIToken(ctx context.Context, id int64) error
	ListAPITokens(ctx context.Context) ([]store.APIToken, error)
	RevokeAPIToken(ctx context.Context, name string) (store.APIToken, error)

	CreateSession(ctx context.Context, s store.NewSession) (store.Session, error)
	FindSessionByHash(ctx context.Context, hash [32]byte) (store.Session, error)
	DeleteSessionByHash(ctx context.Context, hash [32]byte) (store.Session, error)
	ListSessions(ctx context.Context) ([]store.Session, error)
	DeleteUserSessions(ctx context.Context, user string) (int64, error)
	DeleteAllSessions(ctx context.Context) (int64, error)
	DeleteExpiredAuthRows(ctx context.Context) (int64, error)
	CreateLogin(ctx context.Context, l store.NewLogin) error
	ConsumeLogin(ctx context.Context, stateHash [32]byte) (store.Login, error)

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
