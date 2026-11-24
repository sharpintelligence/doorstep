package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func ExecSQL(ctx context.Context, s *Store, sql string, args ...any) error {
	_, err := s.db.Exec(ctx, sql, args...)
	return err
}

func QueryRowSQL(ctx context.Context, s *Store, sql string, args ...any) pgx.Row {
	return s.db.QueryRow(ctx, sql, args...)
}
