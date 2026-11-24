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

const claimAttempts = 3

type Claim struct {
	Attempt        int
	LeaseExpiresAt time.Time
}

const claimSQL = `
WITH head AS (
    SELECT q.deployment_id
    FROM queue_entries q
    JOIN apps a ON a.id = q.app_id
    WHERE q.available_at <= now()
      AND (q.lease_expires_at IS NULL OR q.lease_expires_at <= now())
      AND NOT EXISTS (
          SELECT 1
          FROM queue_entries o
          WHERE o.app_id = q.app_id
            AND o.deployment_id <> q.deployment_id
            AND (
                o.lease_expires_at > now()
                OR o.priority > q.priority
                OR (o.priority = q.priority AND o.deployment_id < q.deployment_id)
            )
      )
    ORDER BY q.priority DESC, q.deployment_id
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

func (s *Store) Claim(ctx context.Context, workerID string, lease time.Duration) (core.Deployment, Claim, bool, error) {
	if strings.TrimSpace(workerID) == "" {
		return core.Deployment{}, Claim{}, false, errors.New("claim: worker ID must not be empty")
	}
	if lease <= 0 {
		return core.Deployment{}, Claim{}, false, fmt.Errorf("claim: lease must be positive, got %s", lease)
	}

	for range claimAttempts {
		d, c, ok, err := s.claimOnce(ctx, workerID, lease)
		if errors.Is(err, errLostClaimRace) {
			continue
		}
		return d, c, ok, err
	}
	return core.Deployment{}, Claim{}, false, nil
}

func (s *Store) claimOnce(ctx context.Context, workerID string, lease time.Duration) (core.Deployment, Claim, bool, error) {
	var (
		d     core.Deployment
		claim Claim
		found bool
	)
	err := s.InTx(ctx, func(tx *Store) error {
		var (
			id    core.DeploymentID
			appID AppID
		)
		err := tx.db.QueryRow(ctx, claimSQL, workerID, lease).Scan(&id, &appID, &claim.Attempt, &claim.LeaseExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim queue entry: %w", err)
		}

		var contended bool
		if err := tx.db.QueryRow(ctx, otherLiveLeaseSQL, appID, id).Scan(&contended); err != nil {
			return fmt.Errorf("check leases for deployment %d: %w", id, err)
		}
		if contended {
			return errLostClaimRace
		}

		d, err = tx.GetDeployment(ctx, id)
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return core.Deployment{}, Claim{}, false, err
	}
	return d, claim, found, nil
}
