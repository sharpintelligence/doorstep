package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

var ErrClaimRejected = errClaimRejected

func ExecSQL(ctx context.Context, s *Store, sql string, args ...any) error {
	_, err := s.db.Exec(ctx, sql, args...)
	return err
}

func QueryRowSQL(ctx context.Context, s *Store, sql string, args ...any) pgx.Row {
	return s.db.QueryRow(ctx, sql, args...)
}

func QueryRowsSQL(ctx context.Context, s *Store, sql string, args ...any) ([]string, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func ClaimExcluding(ctx context.Context, s *Store, workerID string, lease time.Duration, excluded []AppID) (core.Deployment, bool, error) {
	r, err := s.claimOnce(ctx, workerID, lease, excluded, nil)
	return r.deployment, r.found, err
}
