package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func (s *Store) InTx(ctx context.Context, fn func(tx *Store) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		return fn(&Store{db: tx})
	})
}
