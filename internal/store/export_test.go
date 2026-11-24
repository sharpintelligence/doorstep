package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func ClaimRejectReason(err error) (SkipReason, bool) {
	rejected, ok := errors.AsType[*claimRejectedError](err)
	if !ok {
		return "", false
	}
	return rejected.reason, true
}

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

const globalFreezeWaitersSQL = `
SELECT count(*)
FROM pg_locks
WHERE locktype = 'advisory'
  AND NOT granted
  AND objsubid = 1
  AND ((classid::bigint << 32) | objid::bigint) = ` + globalFreezeKey

func GlobalFreezeWaiters(ctx context.Context, s *Store) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, globalFreezeWaitersSQL).Scan(&n)
	return n, err
}
