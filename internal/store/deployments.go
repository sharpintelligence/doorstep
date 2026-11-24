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
    parent_id, rollback_of_id, failure_reason,
    migration, migration_reason
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''),
    $13, $14, $15,
    $16, $17
)
RETURNING id`

const insertQueueEntrySQL = `
INSERT INTO queue_entries (deployment_id, app_id, priority) VALUES ($1, $2, $3)`

const insertEventSQL = `
INSERT INTO deployment_events (deployment_id, app_id, type, from_state, to_state, actor, message, data)
VALUES ($1, $2, $3, NULLIF($4::text, ''), NULLIF($5::text, ''), $6, $7, $8::jsonb)`

const deploymentColumns = `
    d.id, a.name, a.environment, d.kind, d.state, d.source, d.requested_by,
    d.source_repository, d.commit_sha, d.image_repository, d.image_tag,
    COALESCE(d.image_digest, ''), COALESCE(d.previous_digest, ''), COALESCE(d.pin_commit_sha, ''),
    COALESCE(d.gitops_repository, ''), COALESCE(d.argocd_revision, ''),
    d.parent_id, d.rollback_of_id, d.superseded_by_id, d.failure_reason,
    d.migration, d.migration_reason,
    d.created_at, d.updated_at, d.started_at, d.finished_at`

const deploymentTables = `
FROM deployments d
JOIN apps a ON a.id = d.app_id`

const selectDeploymentSQL = `
SELECT` + deploymentColumns + deploymentTables + `
WHERE d.id = $1`

func deploymentFields(d *core.Deployment) []any {
	return []any{
		&d.ID, &d.Target.App, &d.Target.Environment, &d.Kind, &d.State, &d.Source, &d.RequestedBy,
		&d.Commit.Repository, &d.Commit.SHA, &d.Image.Repository, &d.Image.Tag,
		&d.Digest, &d.PreviousDigest, &d.PinCommitSHA,
		&d.GitOpsRepository, &d.ArgoCDRevision,
		&d.ParentID, &d.RollbackOfID, &d.SupersededByID, &d.FailureReason,
		&d.Migration, &d.MigrationReason,
		&d.CreatedAt, &d.UpdatedAt, &d.StartedAt, &d.FinishedAt,
	}
}

type event struct {
	deploymentID core.DeploymentID
	appID        AppID
	typ          core.EventType
	from, to     core.State
	actor        string
	message      string
	data         []byte
}

func (s *Store) insertEvent(ctx context.Context, e event) error {
	data := e.data
	if len(data) == 0 {
		data = []byte("{}")
	}
	_, err := s.db.Exec(ctx, insertEventSQL, e.deploymentID, e.appID, e.typ, e.from, e.to, e.actor, e.message, string(data))
	if err != nil {
		return fmt.Errorf("record deployment %d %s event: %w", e.deploymentID, e.typ, err)
	}
	return nil
}

func scanDeployment(row pgx.Row) (core.Deployment, error) {
	var d core.Deployment
	err := row.Scan(deploymentFields(&d)...)
	return d, err
}

func (s *Store) queryDeployments(ctx context.Context, sql string, args ...any) ([]core.Deployment, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.Deployment, error) {
		return scanDeployment(row)
	})
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
			d.Migration, d.MigrationReason,
		).Scan(&id)
		if err != nil {
			return fmt.Errorf("insert deployment: %w", err)
		}
		if _, err := tx.db.Exec(ctx, insertQueueEntrySQL, id, appID, priority); err != nil {
			return fmt.Errorf("enqueue deployment %d: %w", id, err)
		}
		err = tx.insertEvent(ctx, event{
			deploymentID: id,
			appID:        appID,
			typ:          core.EventTransition,
			to:           d.State,
			actor:        d.RequestedBy,
			message:      "queued from " + string(d.Source),
		})
		if err != nil {
			return err
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
	d, err := scanDeployment(s.db.QueryRow(ctx, selectDeploymentSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Deployment{}, fmt.Errorf("deployment %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return core.Deployment{}, fmt.Errorf("get deployment %d: %w", id, err)
	}
	return d, nil
}
