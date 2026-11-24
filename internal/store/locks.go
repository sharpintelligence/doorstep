package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	acquireFreezeAttempts = 3

	lockColumns = `
    l.app_id IS NULL, COALESCE(a.name, ''), COALESCE(a.environment, ''),
    l.kind, l.owner, l.reason, l.deployment_id, l.created_at, l.expires_at, l.reminded_at`

	liveLock = `(l.expires_at IS NULL OR l.expires_at > now())`

	lockOrder = `ORDER BY l.app_id IS NULL DESC, a.name, a.environment`
)

const globalFreezeKey = `hashtextextended('doorstep:global-freeze:' || current_schema(), 0)`

const (
	lockGlobalFreezeSQL       = `SELECT pg_advisory_xact_lock(` + globalFreezeKey + `)`
	lockGlobalFreezeSharedSQL = `SELECT pg_advisory_xact_lock_shared(` + globalFreezeKey + `)`
)

const purgeExpiredFreezeSQL = `
WITH l AS (
    DELETE FROM locks
    WHERE kind = 'freeze'
      AND app_id IS NOT DISTINCT FROM $1::bigint
      AND expires_at <= now()
    RETURNING *
), event AS (
    INSERT INTO lock_events (app_id, action, owner, reason, expires_at)
    SELECT app_id, 'expired', owner, reason, expires_at FROM l
)
SELECT ` + lockColumns + `
FROM l LEFT JOIN apps a ON a.id = l.app_id`

const insertFreezeSQL = `
WITH l AS (
    INSERT INTO locks (app_id, kind, owner, reason, expires_at)
    VALUES ($1::bigint, 'freeze', $2, $3, COALESCE($4::timestamptz, now() + $5::interval))
    ON CONFLICT DO NOTHING
    RETURNING *
), event AS (
    INSERT INTO lock_events (app_id, action, owner, actor, reason, expires_at)
    SELECT app_id, 'locked', owner, owner, reason, expires_at FROM l
)
SELECT ` + lockColumns + `
FROM l LEFT JOIN apps a ON a.id = l.app_id`

const selectFreezeSQL = `
SELECT ` + lockColumns + `
FROM locks l LEFT JOIN apps a ON a.id = l.app_id
WHERE l.kind = 'freeze'
  AND l.app_id IS NOT DISTINCT FROM $1::bigint
  AND ` + liveLock

const deleteFreezeSQL = `
WITH l AS (
    DELETE FROM locks
    WHERE kind = 'freeze'
      AND app_id IS NOT DISTINCT FROM $1::bigint
      AND (expires_at IS NULL OR expires_at > now())
    RETURNING *
), event AS (
    INSERT INTO lock_events (app_id, action, owner, actor, reason, expires_at)
    SELECT app_id, 'unlocked', owner, $2, reason, expires_at FROM l
)
SELECT ` + lockColumns + `
FROM l LEFT JOIN apps a ON a.id = l.app_id`

const listFreezesSQL = `
SELECT ` + lockColumns + `
FROM locks l LEFT JOIN apps a ON a.id = l.app_id
WHERE l.kind = 'freeze'
  AND ` + liveLock + `
` + lockOrder

const blockingFreezesSQL = `
SELECT ` + lockColumns + `
FROM locks l LEFT JOIN apps a ON a.id = l.app_id
WHERE l.kind = 'freeze'
  AND ` + liveLock + `
  AND (l.app_id IS NULL OR (a.name = $1 AND a.environment = $2))
` + lockOrder

const deleteExpiredFreezesSQL = `
WITH l AS (
    DELETE FROM locks
    WHERE kind = 'freeze'
      AND expires_at <= now()
    RETURNING *
), event AS (
    INSERT INTO lock_events (app_id, action, owner, reason, expires_at)
    SELECT app_id, 'expired', owner, reason, expires_at FROM l
)
SELECT ` + lockColumns + `
FROM l LEFT JOIN apps a ON a.id = l.app_id
` + lockOrder

const claimLockRemindersSQL = `
WITH l AS (
    UPDATE locks
    SET reminded_at = now()
    WHERE kind = 'freeze'
      AND (expires_at IS NULL OR expires_at > now())
      AND COALESCE(reminded_at, created_at) <= now() - $1::interval
    RETURNING *
), event AS (
    INSERT INTO lock_events (app_id, action, owner, reason, expires_at)
    SELECT app_id, 'reminded', owner, reason, expires_at FROM l
)
SELECT ` + lockColumns + `
FROM l LEFT JOIN apps a ON a.id = l.app_id
` + lockOrder

const listLockEventsSQL = `
SELECT e.action, e.app_id IS NULL, COALESCE(a.name, ''), COALESCE(a.environment, ''),
       e.owner, e.actor, e.reason, e.expires_at, e.occurred_at
FROM lock_events e LEFT JOIN apps a ON a.id = e.app_id
ORDER BY e.id DESC
LIMIT $1`

const findAppSQL = `SELECT id FROM apps WHERE name = $1 AND environment = $2`

var errFreezeVanished = fmt.Errorf("%w: the conflicting freeze was released while acquiring", core.ErrLockContended)

func (s *Store) AcquireFreeze(ctx context.Context, req core.LockRequest) (core.Lock, []core.Lock, error) {
	if err := validateFreezeRequest(req); err != nil {
		return core.Lock{}, nil, err
	}
	var lock core.Lock
	var expired []core.Lock
	err := s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.lockScope(ctx, req.Target)
		if err != nil {
			return err
		}
		expired, err = tx.queryLocks(ctx, purgeExpiredFreezeSQL, appID)
		if err != nil {
			return fmt.Errorf("delete expired freeze: %w", err)
		}
		for range acquireFreezeAttempts {
			lock, err = tx.insertFreeze(ctx, appID, req)
			if !errors.Is(err, errFreezeVanished) {
				return err
			}
		}
		return err
	})
	if err != nil {
		return core.Lock{}, nil, fmt.Errorf("lock %s: %w", req.Scope(), err)
	}
	return lock, expired, nil
}

func validateFreezeRequest(req core.LockRequest) error {
	var errs []error
	if req.Target != nil {
		errs = append(errs, req.Target.Validate())
	}
	if strings.TrimSpace(req.Owner) == "" {
		errs = append(errs, fmt.Errorf("%w: owner must not be empty", core.ErrInvalidLock))
	}
	if req.ExpiresIn != nil && req.ExpiresAt != nil {
		errs = append(errs, fmt.Errorf("%w: set at most one of an expiry duration and an expiry time", core.ErrInvalidLock))
	}
	if req.ExpiresIn != nil && *req.ExpiresIn <= 0 {
		errs = append(errs, fmt.Errorf("%w: expiry duration must be positive, got %s", core.ErrInvalidLock, *req.ExpiresIn))
	}
	return errors.Join(errs...)
}

func (s *Store) lockScope(ctx context.Context, target *core.Target) (*AppID, error) {
	if target == nil {
		if _, err := s.db.Exec(ctx, lockGlobalFreezeSQL); err != nil {
			return nil, fmt.Errorf("wait for in-flight claims: %w", err)
		}
		return nil, nil
	}
	id, err := s.EnsureApp(ctx, *target)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx, lockAppSQL, id); err != nil {
		return nil, fmt.Errorf("lock app %s: %w", target, err)
	}
	return &id, nil
}

func (s *Store) insertFreeze(ctx context.Context, appID *AppID, req core.LockRequest) (core.Lock, error) {
	lock, err := scanLock(s.db.QueryRow(ctx, insertFreezeSQL, appID, req.Owner, req.Reason, req.ExpiresAt, req.ExpiresIn))
	if err == nil {
		return lock, nil
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.ConstraintName == "locks_expires_at_check" {
		return core.Lock{}, fmt.Errorf("%w: expiry must be in the future", core.ErrInvalidLock)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return core.Lock{}, fmt.Errorf("insert freeze: %w", err)
	}
	held, err := scanLock(s.db.QueryRow(ctx, selectFreezeSQL, appID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Lock{}, errFreezeVanished
	}
	if err != nil {
		return core.Lock{}, fmt.Errorf("load held freeze: %w", err)
	}
	return core.Lock{}, &core.LockHeldError{Lock: held}
}

func (s *Store) ReleaseFreeze(ctx context.Context, target *core.Target, actor string) (core.Lock, error) {
	scope := core.LockRequest{Target: target}.Scope()
	if target != nil {
		if err := target.Validate(); err != nil {
			return core.Lock{}, fmt.Errorf("unlock %s: %w", scope, err)
		}
	}
	var lock core.Lock
	err := s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.findApp(ctx, target)
		if err != nil {
			return err
		}
		lock, err = scanLock(tx.db.QueryRow(ctx, deleteFreezeSQL, appID, actor))
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("delete freeze: %w", err)
		}
		return nil
	})
	if err != nil {
		return core.Lock{}, fmt.Errorf("unlock %s: %w", scope, err)
	}
	return lock, nil
}

func (s *Store) findApp(ctx context.Context, target *core.Target) (*AppID, error) {
	if target == nil {
		return nil, nil
	}
	var id AppID
	err := s.db.QueryRow(ctx, findAppSQL, target.App, target.Environment).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find app %s: %w", target, err)
	}
	return &id, nil
}

func (s *Store) ListFreezes(ctx context.Context) ([]core.Lock, error) {
	locks, err := s.queryLocks(ctx, listFreezesSQL)
	if err != nil {
		return nil, fmt.Errorf("list freezes: %w", err)
	}
	return locks, nil
}

func (s *Store) BlockingFreezes(ctx context.Context, t core.Target) ([]core.Lock, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	locks, err := s.queryLocks(ctx, blockingFreezesSQL, t.App, t.Environment)
	if err != nil {
		return nil, fmt.Errorf("list freezes blocking %s: %w", t, err)
	}
	return locks, nil
}

func (s *Store) DeleteExpiredFreezes(ctx context.Context) ([]core.Lock, error) {
	locks, err := s.queryLocks(ctx, deleteExpiredFreezesSQL)
	if err != nil {
		return nil, fmt.Errorf("delete expired freezes: %w", err)
	}
	return locks, nil
}

func (s *Store) ClaimLockReminders(ctx context.Context, every time.Duration) ([]core.Lock, error) {
	if every <= 0 {
		return nil, fmt.Errorf("claim lock reminders: interval must be positive, got %s", every)
	}
	locks, err := s.queryLocks(ctx, claimLockRemindersSQL, every)
	if err != nil {
		return nil, fmt.Errorf("claim lock reminders: %w", err)
	}
	return locks, nil
}

func (s *Store) ListLockEvents(ctx context.Context, limit int) ([]core.LockEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("list lock events: limit must be positive, got %d", limit)
	}
	rows, err := s.db.Query(ctx, listLockEventsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("list lock events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.LockEvent, error) {
		ev := core.LockEvent{Lock: core.Lock{Kind: core.LockFreeze}}
		err := row.Scan(&ev.Action, &ev.Lock.Global, &ev.Lock.Target.App, &ev.Lock.Target.Environment,
			&ev.Lock.Owner, &ev.Actor, &ev.Lock.Reason, &ev.Lock.ExpiresAt, &ev.OccurredAt)
		return ev, err
	})
	if err != nil {
		return nil, fmt.Errorf("list lock events: %w", err)
	}
	return events, nil
}

func (s *Store) queryLocks(ctx context.Context, sql string, args ...any) ([]core.Lock, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.Lock, error) {
		return scanLock(row)
	})
}

func scanLock(row pgx.Row) (core.Lock, error) {
	var l core.Lock
	err := row.Scan(&l.Global, &l.Target.App, &l.Target.Environment,
		&l.Kind, &l.Owner, &l.Reason, &l.DeploymentID, &l.CreatedAt, &l.ExpiresAt, &l.RemindedAt)
	return l, err
}
