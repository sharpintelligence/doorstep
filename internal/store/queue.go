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

const lockAppSQL = `SELECT id FROM apps WHERE id = $1 FOR NO KEY UPDATE`

const findDuplicateDeploySQL = `
SELECT d.id, d.state, q.priority
FROM deployments d
LEFT JOIN queue_entries q ON q.deployment_id = d.id
WHERE d.app_id = $1
  AND d.kind = 'deploy'
  AND d.commit_sha = $2
  AND d.state = ANY($3::text[])
ORDER BY d.id
LIMIT 1`

const deploymentAppSQL = `SELECT app_id FROM deployments WHERE id = $1`

const queuedDeploySQL = `
SELECT d.state, d.kind, q.priority
FROM deployments d
JOIN queue_entries q ON q.deployment_id = d.id
WHERE d.id = $1
FOR NO KEY UPDATE OF q`

const raisePrioritySQL = `UPDATE queue_entries SET priority = $2 WHERE deployment_id = $1`

const insertNoteEventSQL = `
INSERT INTO deployment_events (deployment_id, app_id, type, actor, message)
VALUES ($1, $2, 'note', $3, $4)`

const heartbeatSQL = `
UPDATE queue_entries
SET lease_expires_at = now() + $3::interval
WHERE deployment_id = $1
  AND lease_owner = $2
  AND lease_expires_at > now()
RETURNING lease_expires_at`

const ownedEntryStateSQL = `
SELECT d.state
FROM queue_entries q
JOIN deployments d ON d.id = q.deployment_id
WHERE q.deployment_id = $1
  AND q.lease_owner = $2
FOR NO KEY UPDATE OF q`

const releaseSQL = `
UPDATE queue_entries
SET lease_owner = NULL,
    lease_expires_at = NULL,
    available_at = now() + $3::interval
WHERE deployment_id = $1
  AND lease_owner = $2`

const completeSQL = `DELETE FROM queue_entries WHERE deployment_id = $1 AND lease_owner = $2`

const queueEntriesSQL = `
SELECT` + deploymentColumns + `,
    q.priority, q.enqueued_at, q.available_at,
    CASE WHEN q.lease_expires_at > now() THEN q.lease_expires_at END,
    q.attempts
FROM queue_entries q
JOIN deployments d ON d.id = q.deployment_id
JOIN apps a ON a.id = q.app_id
WHERE a.name = $1
  AND d.state = ANY($2::text[])
ORDER BY a.environment,
    (d.state <> 'queued' OR COALESCE(q.lease_expires_at > now(), false)) DESC,
    q.priority DESC,
    q.deployment_id`

const queueDepthSQL = `
SELECT count(*)
FROM queue_entries q
JOIN deployments d ON d.id = q.deployment_id
WHERE d.state = 'queued'
  AND (q.lease_expires_at IS NULL OR q.lease_expires_at <= now())`

func (s *Store) EnqueueDeployment(ctx context.Context, d core.Deployment, priority core.Priority) (core.Deployment, bool, error) {
	d, err := newDeployment(d)
	if err != nil {
		return core.Deployment{}, false, fmt.Errorf("enqueue deployment: %w", err)
	}

	var result core.Deployment
	var created bool
	err = s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.EnsureApp(ctx, d.Target)
		if err != nil {
			return err
		}
		if err := tx.lockApp(ctx, appID); err != nil {
			return err
		}
		if d.Kind == core.KindDeploy {
			existing, found, err := tx.findDuplicateDeploy(ctx, appID, d, priority)
			if err != nil || found {
				result = existing
				return err
			}
		}
		result, err = tx.CreateDeployment(ctx, d, int(priority))
		created = err == nil
		return err
	})
	if err != nil {
		return core.Deployment{}, false, fmt.Errorf("enqueue deployment for %s: %w", d.Target, err)
	}
	return result, created, nil
}

func (s *Store) findDuplicateDeploy(ctx context.Context, appID AppID, d core.Deployment, priority core.Priority) (core.Deployment, bool, error) {
	var (
		id       core.DeploymentID
		state    core.State
		existing *int
	)
	err := s.db.QueryRow(ctx, findDuplicateDeploySQL, appID, d.Commit.SHA, core.ClaimableStates()).Scan(&id, &state, &existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Deployment{}, false, nil
	}
	if err != nil {
		return core.Deployment{}, false, fmt.Errorf("find deploy of commit %s: %w", d.Commit.SHA, err)
	}
	if state == core.StateQueued && existing != nil && core.Priority(*existing) < priority {
		if err := s.raisePriority(ctx, appID, id, priority, d.RequestedBy); err != nil {
			return core.Deployment{}, false, err
		}
	}
	found, err := s.GetDeployment(ctx, id)
	return found, err == nil, err
}

func (s *Store) ExpediteDeployment(ctx context.Context, id core.DeploymentID, priority core.Priority, actor string) error {
	err := s.InTx(ctx, func(tx *Store) error {
		var appID AppID
		err := tx.db.QueryRow(ctx, deploymentAppSQL, id).Scan(&appID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("deployment %d: %w", id, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read deployment %d: %w", id, err)
		}
		if err := tx.lockApp(ctx, appID); err != nil {
			return err
		}

		var (
			state   core.State
			kind    core.Kind
			current int
		)
		err = tx.db.QueryRow(ctx, queuedDeploySQL, id).Scan(&state, &kind, &current)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (state != core.StateQueued || kind != core.KindDeploy)) {
			return fmt.Errorf("queued deploy %d: %w", id, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read queue entry of deployment %d: %w", id, err)
		}
		if core.Priority(current) >= priority {
			return nil
		}
		return tx.raisePriority(ctx, appID, id, priority, actor)
	})
	if err != nil {
		return fmt.Errorf("expedite deployment: %w", err)
	}
	return nil
}

func (s *Store) raisePriority(ctx context.Context, appID AppID, id core.DeploymentID, priority core.Priority, actor string) error {
	if _, err := s.db.Exec(ctx, raisePrioritySQL, id, int(priority)); err != nil {
		return fmt.Errorf("raise priority of deployment %d: %w", id, err)
	}
	message := "priority raised to " + priority.String()
	if _, err := s.db.Exec(ctx, insertNoteEventSQL, id, appID, actor, message); err != nil {
		return fmt.Errorf("record priority of deployment %d: %w", id, err)
	}
	return nil
}

func (s *Store) lockApp(ctx context.Context, appID AppID) error {
	var locked AppID
	if err := s.db.QueryRow(ctx, lockAppSQL, appID).Scan(&locked); err != nil {
		return fmt.Errorf("lock app %d: %w", appID, err)
	}
	return nil
}

func (s *Store) HeartbeatQueueEntry(ctx context.Context, id core.DeploymentID, workerID string, lease time.Duration) (time.Time, error) {
	if err := validateClaimArgs(workerID, lease); err != nil {
		return time.Time{}, fmt.Errorf("heartbeat: %w", err)
	}
	var expires time.Time
	err := s.db.QueryRow(ctx, heartbeatSQL, id, workerID, lease).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("heartbeat deployment %d: %w", id, core.ErrLeaseLost)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("heartbeat deployment %d: %w", id, err)
	}
	return expires, nil
}

func (s *Store) ReleaseQueueEntry(ctx context.Context, id core.DeploymentID, workerID string, delay time.Duration) error {
	if strings.TrimSpace(workerID) == "" {
		return errors.New("release: worker ID must not be empty")
	}
	if delay < 0 {
		return fmt.Errorf("release: delay must not be negative, got %s", delay)
	}
	err := s.InTx(ctx, func(tx *Store) error {
		var state core.State
		err := tx.db.QueryRow(ctx, ownedEntryStateSQL, id, workerID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return core.ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if state.Terminal() {
			_, err = tx.db.Exec(ctx, completeSQL, id, workerID)
			return err
		}
		_, err = tx.db.Exec(ctx, releaseSQL, id, workerID, delay)
		return err
	})
	if err != nil {
		return fmt.Errorf("release deployment %d: %w", id, err)
	}
	return nil
}

func (s *Store) CompleteQueueEntry(ctx context.Context, id core.DeploymentID, workerID string) error {
	if strings.TrimSpace(workerID) == "" {
		return errors.New("complete: worker ID must not be empty")
	}
	return s.execOwned(ctx, "complete", id, completeSQL, id, workerID)
}

func (s *Store) execOwned(ctx context.Context, op string, id core.DeploymentID, sql string, args ...any) error {
	tag, err := s.db.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("%s deployment %d: %w", op, id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s deployment %d: %w", op, id, core.ErrLeaseLost)
	}
	return nil
}

func (s *Store) QueueEntries(ctx context.Context, app string) ([]core.QueueEntry, error) {
	rows, err := s.db.Query(ctx, queueEntriesSQL, app, core.ClaimableStates())
	if err != nil {
		return nil, fmt.Errorf("list queue of %s: %w", app, err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.QueueEntry, error) {
		var e core.QueueEntry
		fields := append(deploymentFields(&e.Deployment), &e.Priority, &e.EnqueuedAt, &e.AvailableAt, &e.LeaseExpiresAt, &e.Attempts)
		err := row.Scan(fields...)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("list queue of %s: %w", app, err)
	}
	position, environment := 0, ""
	for i := range entries {
		if entries[i].Deployment.Target.Environment != environment {
			position, environment = 0, entries[i].Deployment.Target.Environment
		}
		position++
		entries[i].Position = position
	}
	return entries, nil
}

func (s *Store) QueueDepth(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRow(ctx, queueDepthSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("count queued deployments: %w", err)
	}
	return n, nil
}
