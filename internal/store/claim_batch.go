package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const supersedeActor = "doorstep"

type BatchLimit func(core.Target) int

type ClaimedBatch struct {
	Deployment core.Deployment
	Claim      Claim
	Superseded []core.DeploymentID
}

const headPrioritySQL = `SELECT priority FROM queue_entries WHERE deployment_id = $1`

const batchWindowSQL = `
SELECT q.deployment_id
FROM queue_entries q
JOIN deployments d ON d.id = q.deployment_id
WHERE q.app_id = $1
  AND q.priority = $2
  AND q.deployment_id >= $3
  AND d.state = 'queued'
  AND d.kind = 'deploy'
ORDER BY q.deployment_id
LIMIT $4
FOR NO KEY UPDATE OF q`

const olderLowerPrioritySQL = `
SELECT q.deployment_id
FROM queue_entries q
JOIN deployments d ON d.id = q.deployment_id
WHERE q.app_id = $1
  AND q.priority < $2
  AND q.deployment_id < $3
  AND d.state = 'queued'
  AND d.kind = 'deploy'
ORDER BY q.deployment_id
FOR NO KEY UPDATE OF q`

const supersedeSQL = `
UPDATE deployments
SET state = 'superseded', superseded_by_id = $2, finished_at = now()
WHERE id = ANY($1::bigint[]) AND state = 'queued'
RETURNING id`

const deleteQueueEntriesSQL = `DELETE FROM queue_entries WHERE deployment_id = ANY($1::bigint[])`

const insertSupersededEventsSQL = `
INSERT INTO deployment_events (deployment_id, app_id, type, from_state, to_state, actor, message)
SELECT id, $2, 'transition', 'queued', 'superseded', $3, $4
FROM unnest($1::bigint[]) WITH ORDINALITY AS s (id, n)
ORDER BY n`

const leaseWinnerSQL = `
UPDATE queue_entries
SET lease_owner = $2,
    lease_expires_at = now() + $3::interval,
    attempts = attempts + 1,
    available_at = LEAST(available_at, now())
WHERE deployment_id = $1
RETURNING attempts, lease_expires_at`

func (s *Store) ClaimBatch(ctx context.Context, workerID string, lease time.Duration, limit BatchLimit) (ClaimedBatch, bool, error) {
	if err := validateClaimArgs(workerID, lease); err != nil {
		return ClaimedBatch{}, false, fmt.Errorf("claim batch: %w", err)
	}
	if limit == nil {
		limit = func(core.Target) int { return 1 }
	}
	r, err := s.claimRetrying(ctx, workerID, lease, limit)
	if err != nil || !r.found {
		return ClaimedBatch{}, false, err
	}
	return ClaimedBatch{Deployment: r.deployment, Claim: r.claim, Superseded: r.superseded}, true, nil
}

func (s *Store) batch(ctx context.Context, r *claimResult, workerID string, lease time.Duration, limit BatchLimit) error {
	head := r.deployment
	if head.State != core.StateQueued || head.Kind != core.KindDeploy {
		return nil
	}
	var priority int
	if err := s.db.QueryRow(ctx, headPrioritySQL, head.ID).Scan(&priority); err != nil {
		return fmt.Errorf("read priority of deployment %d: %w", head.ID, err)
	}

	window, err := s.deploymentIDs(ctx, batchWindowSQL, r.appID, priority, head.ID, max(limit(head.Target), 1))
	if err != nil {
		return fmt.Errorf("select batch for deployment %d: %w", head.ID, err)
	}
	if len(window) == 0 {
		return nil
	}
	winner := window[len(window)-1]
	older, err := s.deploymentIDs(ctx, olderLowerPrioritySQL, r.appID, priority, winner)
	if err != nil {
		return fmt.Errorf("select deploys older than deployment %d: %w", winner, err)
	}
	candidates := slices.Concat(window[:len(window)-1], older)
	slices.Sort(candidates)

	superseded, err := s.supersede(ctx, r.appID, candidates, winner)
	if err != nil {
		return err
	}
	r.superseded = superseded
	if winner == head.ID {
		return nil
	}

	if err := s.db.QueryRow(ctx, leaseWinnerSQL, winner, workerID, lease).Scan(&r.claim.Attempt, &r.claim.LeaseExpiresAt); err != nil {
		return fmt.Errorf("lease deployment %d: %w", winner, err)
	}
	r.deployment, err = s.GetDeployment(ctx, winner)
	return err
}

func (s *Store) supersede(ctx context.Context, appID AppID, ids []core.DeploymentID, winner core.DeploymentID) ([]core.DeploymentID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	superseded, err := s.deploymentIDs(ctx, supersedeSQL, ids, winner)
	if err != nil {
		return nil, fmt.Errorf("supersede deployments by %d: %w", winner, err)
	}
	if len(superseded) == 0 {
		return nil, nil
	}
	slices.Sort(superseded)
	if _, err := s.db.Exec(ctx, deleteQueueEntriesSQL, superseded); err != nil {
		return nil, fmt.Errorf("dequeue deployments superseded by %d: %w", winner, err)
	}
	message := fmt.Sprintf("superseded by deployment %d", winner)
	if _, err := s.db.Exec(ctx, insertSupersededEventsSQL, superseded, appID, supersedeActor, message); err != nil {
		return nil, fmt.Errorf("record deployments superseded by %d: %w", winner, err)
	}
	return superseded, nil
}

func (s *Store) deploymentIDs(ctx context.Context, sql string, args ...any) ([]core.DeploymentID, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[core.DeploymentID])
}
