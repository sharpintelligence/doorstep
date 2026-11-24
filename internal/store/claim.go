package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Claim struct {
	Attempt        int
	LeaseExpiresAt time.Time
	Skipped        []SkippedTarget
}

type SkipReason string

const (
	SkipLeased SkipReason = "leased"
	SkipFrozen SkipReason = "frozen"
)

type SkippedTarget struct {
	AppID  AppID
	Reason SkipReason
}

const claimSQL = `
WITH head AS (
    SELECT q.deployment_id
    FROM queue_entries q
    JOIN deployments d ON d.id = q.deployment_id
    JOIN apps a ON a.id = q.app_id
    WHERE d.state = ANY($3::text[])
      AND NOT q.app_id = ANY($4::bigint[])
      AND q.available_at <= now()
      AND (q.lease_expires_at IS NULL OR q.lease_expires_at <= now())
      AND (d.state <> $5 OR d.kind = $6 OR NOT EXISTS (
          SELECT 1
          FROM locks l
          WHERE l.kind = $7
            AND (l.app_id = q.app_id OR l.app_id IS NULL)
            AND (l.expires_at IS NULL OR l.expires_at > now())
      ))
      AND NOT EXISTS (
          SELECT 1
          FROM queue_entries o
          JOIN deployments od ON od.id = o.deployment_id
          WHERE o.app_id = q.app_id
            AND o.deployment_id <> q.deployment_id
            AND (
                o.lease_expires_at > now()
                OR (
                    od.state = ANY($3::text[])
                    AND (od.state <> 'queued', o.priority, -o.deployment_id)
                        > (d.state <> 'queued', q.priority, -q.deployment_id)
                )
            )
      )
    ORDER BY (d.state <> 'queued') DESC, q.priority DESC, q.deployment_id
    LIMIT 1
    FOR NO KEY UPDATE OF q, a SKIP LOCKED
)
UPDATE queue_entries q
SET lease_owner = $1,
    lease_expires_at = now() + $2::interval,
    attempts = q.attempts + 1
FROM head
WHERE q.deployment_id = head.deployment_id
RETURNING q.deployment_id, q.app_id, q.attempts, q.lease_expires_at`

const recheckClaimSQL = `
SELECT
    EXISTS (
        SELECT 1
        FROM queue_entries
        WHERE app_id = $1
          AND deployment_id <> $2
          AND lease_expires_at > now()
    ),
    EXISTS (
        SELECT 1
        FROM deployments d
        WHERE d.id = $2
          AND d.state = $3
          AND d.kind <> $4
          AND EXISTS (
              SELECT 1
              FROM locks l
              WHERE l.kind = $5
                AND (l.app_id = $1 OR l.app_id IS NULL)
                AND (l.expires_at IS NULL OR l.expires_at > now())
          )
    )`

type claimRejectedError struct {
	reason SkipReason
}

func (e *claimRejectedError) Error() string {
	return "target was " + string(e.reason) + " while claiming"
}

type claimResult struct {
	deployment core.Deployment
	claim      Claim
	appID      AppID
	superseded []core.DeploymentID
	found      bool
}

func (s *Store) Claim(ctx context.Context, workerID string, lease time.Duration) (core.Deployment, Claim, bool, error) {
	if err := validateClaimArgs(workerID, lease); err != nil {
		return core.Deployment{}, Claim{}, false, fmt.Errorf("claim: %w", err)
	}

	r, err := s.claimRetrying(ctx, workerID, lease, nil)
	return r.deployment, r.claim, r.found, err
}

func validateClaimArgs(workerID string, lease time.Duration) error {
	if strings.TrimSpace(workerID) == "" {
		return errors.New("worker ID must not be empty")
	}
	if lease <= 0 {
		return fmt.Errorf("lease must be positive, got %s", lease)
	}
	return nil
}

func (s *Store) claimRetrying(ctx context.Context, workerID string, lease time.Duration, limit BatchLimit) (claimResult, error) {
	excluded := []AppID{}
	var skipped []SkippedTarget
	for {
		r, err := s.claimOnce(ctx, workerID, lease, excluded, limit)
		if rejected, ok := errors.AsType[*claimRejectedError](err); ok {
			excluded = append(excluded, r.appID)
			skipped = append(skipped, SkippedTarget{AppID: r.appID, Reason: rejected.reason})
			continue
		}
		r.claim.Skipped = skipped
		return r, err
	}
}

func (s *Store) claimOnce(ctx context.Context, workerID string, lease time.Duration, excluded []AppID, limit BatchLimit) (claimResult, error) {
	var r claimResult
	err := s.InTx(ctx, func(tx *Store) error {
		if _, err := tx.db.Exec(ctx, lockGlobalFreezeSharedSQL); err != nil {
			return fmt.Errorf("wait for global freeze: %w", err)
		}
		var id core.DeploymentID
		err := tx.db.QueryRow(ctx, claimSQL, workerID, lease, core.ClaimableStates(), excluded,
			core.StateQueued, core.KindRollback, core.LockFreeze).
			Scan(&id, &r.appID, &r.claim.Attempt, &r.claim.LeaseExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim queue entry: %w", err)
		}

		var contended, frozen bool
		err = tx.db.QueryRow(ctx, recheckClaimSQL, r.appID, id, core.StateQueued, core.KindRollback, core.LockFreeze).
			Scan(&contended, &frozen)
		if err != nil {
			return fmt.Errorf("recheck claim of deployment %d: %w", id, err)
		}
		switch {
		case contended:
			return &claimRejectedError{reason: SkipLeased}
		case frozen:
			return &claimRejectedError{reason: SkipFrozen}
		}

		r.deployment, err = tx.GetDeployment(ctx, id)
		if err != nil {
			return err
		}
		r.found = true
		if limit == nil {
			return nil
		}
		return tx.batch(ctx, &r, workerID, lease, limit)
	})
	if _, ok := errors.AsType[*claimRejectedError](err); ok {
		return claimResult{appID: r.appID}, err
	}
	if err != nil {
		return claimResult{}, err
	}
	return r, nil
}
