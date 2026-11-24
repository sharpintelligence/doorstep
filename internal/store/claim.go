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

const otherLiveLeaseSQL = `
SELECT EXISTS (
    SELECT 1
    FROM queue_entries
    WHERE app_id = $1
      AND deployment_id <> $2
      AND lease_expires_at > now()
)`

var errLostClaimRace = errors.New("another worker holds a lease on this target")

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
	for {
		r, err := s.claimOnce(ctx, workerID, lease, excluded, limit)
		if errors.Is(err, errLostClaimRace) {
			excluded = append(excluded, r.appID)
			continue
		}
		return r, err
	}
}

func (s *Store) claimOnce(ctx context.Context, workerID string, lease time.Duration, excluded []AppID, limit BatchLimit) (claimResult, error) {
	var r claimResult
	err := s.InTx(ctx, func(tx *Store) error {
		var id core.DeploymentID
		err := tx.db.QueryRow(ctx, claimSQL, workerID, lease, core.ClaimableStates(), excluded).
			Scan(&id, &r.appID, &r.claim.Attempt, &r.claim.LeaseExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim queue entry: %w", err)
		}

		var contended bool
		if err := tx.db.QueryRow(ctx, otherLiveLeaseSQL, r.appID, id).Scan(&contended); err != nil {
			return fmt.Errorf("check leases for deployment %d: %w", id, err)
		}
		if contended {
			return errLostClaimRace
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
	if errors.Is(err, errLostClaimRace) {
		return claimResult{appID: r.appID}, err
	}
	if err != nil {
		return claimResult{}, err
	}
	return r, nil
}
