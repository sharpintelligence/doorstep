package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const insertDeploymentSQL = `
INSERT INTO deployments (
    app_id, kind, state, source, requested_by,
    source_repository, commit_sha, image_repository, image_tag,
    image_digest, previous_digest, pin_commit_sha,
    parent_id, rollback_of_id, failure_reason
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''),
    $13, $14, $15
)
RETURNING id`

const insertQueueEntrySQL = `
INSERT INTO queue_entries (deployment_id, app_id, priority) VALUES ($1, $2, $3)`

const insertTransitionEventSQL = `
INSERT INTO deployment_events (deployment_id, app_id, type, to_state, actor, message)
VALUES ($1, $2, 'transition', $3, $4, $5)`

const deploymentColumns = `
    d.id, a.name, a.environment, d.kind, d.state, d.source, d.requested_by,
    d.source_repository, d.commit_sha, d.image_repository, d.image_tag,
    COALESCE(d.image_digest, ''), COALESCE(d.previous_digest, ''), COALESCE(d.pin_commit_sha, ''),
    d.parent_id, d.rollback_of_id, d.superseded_by_id, d.failure_reason,
    d.created_at, d.updated_at, d.started_at, d.finished_at`

const selectDeploymentSQL = `
SELECT` + deploymentColumns + `
FROM deployments d
JOIN apps a ON a.id = d.app_id
WHERE d.id = $1`

func deploymentFields(d *core.Deployment) []any {
	return []any{
		&d.ID, &d.Target.App, &d.Target.Environment, &d.Kind, &d.State, &d.Source, &d.RequestedBy,
		&d.Commit.Repository, &d.Commit.SHA, &d.Image.Repository, &d.Image.Tag,
		&d.Digest, &d.PreviousDigest, &d.PinCommitSHA,
		&d.ParentID, &d.RollbackOfID, &d.SupersededByID, &d.FailureReason,
		&d.CreatedAt, &d.UpdatedAt, &d.StartedAt, &d.FinishedAt,
	}
}

func newDeployment(d core.Deployment) (core.Deployment, error) {
	if d.State == "" {
		d.State = core.StateQueued
	}
	if d.State != core.StateQueued {
		return core.Deployment{}, fmt.Errorf("new deployments start %s, got %s", core.StateQueued, d.State)
	}
	if d.SupersededByID != nil {
		return core.Deployment{}, errors.New("a new deployment cannot already be superseded")
	}
	if err := d.Validate(); err != nil {
		return core.Deployment{}, err
	}
	return d, nil
}

func (s *Store) CreateDeployment(ctx context.Context, d core.Deployment, priority int) (core.Deployment, error) {
	d, err := newDeployment(d)
	if err != nil {
		return core.Deployment{}, fmt.Errorf("create deployment: %w", err)
	}

	var created core.Deployment
	err = s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.EnsureApp(ctx, d.Target)
		if err != nil {
			return err
		}
		var id core.DeploymentID
		err = tx.db.QueryRow(ctx, insertDeploymentSQL,
			appID, d.Kind, d.State, d.Source, d.RequestedBy,
			d.Commit.Repository, d.Commit.SHA, d.Image.Repository, d.Image.Tag,
			d.Digest, d.PreviousDigest, d.PinCommitSHA,
			d.ParentID, d.RollbackOfID, d.FailureReason,
		).Scan(&id)
		if err != nil {
			return fmt.Errorf("insert deployment: %w", err)
		}
		if _, err := tx.db.Exec(ctx, insertQueueEntrySQL, id, appID, priority); err != nil {
			return fmt.Errorf("enqueue deployment %d: %w", id, err)
		}
		if _, err := tx.db.Exec(ctx, insertTransitionEventSQL, id, appID, d.State, d.RequestedBy, "queued from "+string(d.Source)); err != nil {
			return fmt.Errorf("record deployment %d event: %w", id, err)
		}
		created, err = tx.GetDeployment(ctx, id)
		return err
	})
	if err != nil {
		return core.Deployment{}, fmt.Errorf("create deployment for %s: %w", d.Target, err)
	}
	return created, nil
}

func (s *Store) GetDeployment(ctx context.Context, id core.DeploymentID) (core.Deployment, error) {
	var d core.Deployment
	err := s.db.QueryRow(ctx, selectDeploymentSQL, id).Scan(deploymentFields(&d)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Deployment{}, fmt.Errorf("deployment %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return core.Deployment{}, fmt.Errorf("get deployment %d: %w", id, err)
	}
	return d, nil
}
