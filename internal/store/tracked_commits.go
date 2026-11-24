package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const trackedCommitColumns = `
    t.id, a.name, a.environment, t.repository, t.commit_sha, t.branch, t.source, t.pushed_by,
    t.checks_state, t.checks, t.deployment_id, t.superseded_by_id,
    t.created_at, t.updated_at, t.evaluated_at`

const selectTrackedCommitSQL = `
SELECT` + trackedCommitColumns + `
FROM tracked_commits t
JOIN apps a ON a.id = t.app_id
WHERE t.id = $1`

const selectTrackedCommitsBySHASQL = `
SELECT` + trackedCommitColumns + `
FROM tracked_commits t
JOIN apps a ON a.id = t.app_id
WHERE t.commit_sha = $2 AND lower(t.repository) = lower($1)
ORDER BY t.id`

const selectPendingTrackedCommitsSQL = `
SELECT` + trackedCommitColumns + `
FROM tracked_commits t
JOIN apps a ON a.id = t.app_id
WHERE t.checks_state = 'pending' AND t.deployment_id IS NULL AND t.superseded_by_id IS NULL
ORDER BY t.id
LIMIT $1`

const lockAppSQL = `SELECT id FROM apps WHERE id = $1 FOR NO KEY UPDATE`

const insertTrackedCommitSQL = `
INSERT INTO tracked_commits (app_id, repository, branch, commit_sha, source, pushed_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (app_id, commit_sha) DO NOTHING
RETURNING id`

const selectTrackedCommitIDSQL = `
SELECT id FROM tracked_commits WHERE app_id = $1 AND commit_sha = $2`

const supersedeOlderTrackedCommitsSQL = `
UPDATE tracked_commits
SET superseded_by_id = $2
WHERE app_id = $1
  AND id < $2
  AND deployment_id IS NULL
  AND superseded_by_id IS NULL`

const saveCommitChecksSQL = `
UPDATE tracked_commits
SET checks_state = $2, checks = $3, evaluated_at = now()
WHERE id = $1 AND deployment_id IS NULL AND superseded_by_id IS NULL`

const lockTrackedCommitAppSQL = `
SELECT a.id
FROM apps a
JOIN tracked_commits t ON t.app_id = a.id
WHERE t.id = $1
FOR NO KEY UPDATE OF a`

const lockTrackedCommitSQL = selectTrackedCommitSQL + `
FOR NO KEY UPDATE OF t`

const linkTrackedCommitDeploymentSQL = `
UPDATE tracked_commits SET deployment_id = $2 WHERE id = $1`

func (s *Store) TrackCommit(ctx context.Context, c core.TrackedCommit) (core.TrackedCommit, bool, error) {
	if c.ChecksState == "" {
		c.ChecksState = core.ChecksPending
	}
	if c.ChecksState != core.ChecksPending {
		return core.TrackedCommit{}, false, fmt.Errorf("track commit: new commits start %s, got %s", core.ChecksPending, c.ChecksState)
	}
	if c.DeploymentID != nil || c.SupersededByID != nil {
		return core.TrackedCommit{}, false, errors.New("track commit: a new commit cannot already be enqueued or superseded")
	}
	if err := c.Validate(); err != nil {
		return core.TrackedCommit{}, false, fmt.Errorf("track commit: %w", err)
	}

	var (
		tracked core.TrackedCommit
		created bool
	)
	err := s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.EnsureApp(ctx, c.Target)
		if err != nil {
			return err
		}
		if _, err := tx.db.Exec(ctx, lockAppSQL, appID); err != nil {
			return fmt.Errorf("lock app %s: %w", c.Target, err)
		}
		var id core.TrackedCommitID
		err = tx.db.QueryRow(ctx, insertTrackedCommitSQL,
			appID, c.Commit.Repository, c.Branch, c.Commit.SHA, c.Source, c.PushedBy,
		).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := tx.db.QueryRow(ctx, selectTrackedCommitIDSQL, appID, c.Commit.SHA).Scan(&id); err != nil {
				return fmt.Errorf("find tracked commit: %w", err)
			}
		case err != nil:
			return fmt.Errorf("insert tracked commit: %w", err)
		default:
			created = true
			if _, err := tx.db.Exec(ctx, supersedeOlderTrackedCommitsSQL, appID, id); err != nil {
				return fmt.Errorf("supersede commits older than %d: %w", id, err)
			}
		}
		tracked, err = tx.GetTrackedCommit(ctx, id)
		return err
	})
	if err != nil {
		return core.TrackedCommit{}, false, fmt.Errorf("track commit %s for %s: %w", c.Commit.SHA, c.Target, err)
	}
	return tracked, created, nil
}

func (s *Store) GetTrackedCommit(ctx context.Context, id core.TrackedCommitID) (core.TrackedCommit, error) {
	c, err := scanTrackedCommit(s.db.QueryRow(ctx, selectTrackedCommitSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.TrackedCommit{}, fmt.Errorf("tracked commit %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return core.TrackedCommit{}, fmt.Errorf("get tracked commit %d: %w", id, err)
	}
	return c, nil
}

func (s *Store) TrackedCommits(ctx context.Context, repository, sha string) ([]core.TrackedCommit, error) {
	commits, err := s.queryTrackedCommits(ctx, selectTrackedCommitsBySHASQL, repository, sha)
	if err != nil {
		return nil, fmt.Errorf("tracked commits for %s@%s: %w", repository, sha, err)
	}
	return commits, nil
}

func (s *Store) PendingTrackedCommits(ctx context.Context, limit int) ([]core.TrackedCommit, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("pending tracked commits: limit must be positive, got %d", limit)
	}
	commits, err := s.queryTrackedCommits(ctx, selectPendingTrackedCommitsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("pending tracked commits: %w", err)
	}
	return commits, nil
}

func (s *Store) SaveCommitChecks(ctx context.Context, id core.TrackedCommitID, state core.ChecksState, checks []core.Check) (core.TrackedCommit, error) {
	if !state.Valid() {
		return core.TrackedCommit{}, fmt.Errorf("save checks for tracked commit %d: unknown checks state %q", id, state)
	}
	if checks == nil {
		checks = []core.Check{}
	}
	encoded, err := json.Marshal(checks)
	if err != nil {
		return core.TrackedCommit{}, fmt.Errorf("save checks for tracked commit %d: encode checks: %w", id, err)
	}
	if _, err := s.db.Exec(ctx, saveCommitChecksSQL, id, state, encoded); err != nil {
		return core.TrackedCommit{}, fmt.Errorf("save checks for tracked commit %d: %w", id, err)
	}
	return s.GetTrackedCommit(ctx, id)
}

func (s *Store) PromoteTrackedCommit(ctx context.Context, id core.TrackedCommitID, enqueue func(tx *Store) (core.DeploymentID, error)) (core.TrackedCommit, bool, error) {
	var (
		commit   core.TrackedCommit
		promoted bool
	)
	err := s.InTx(ctx, func(tx *Store) error {
		err := tx.db.QueryRow(ctx, lockTrackedCommitAppSQL, id).Scan(new(AppID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("tracked commit %d: %w", id, core.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("lock app of tracked commit %d: %w", id, err)
		}
		current, err := scanTrackedCommit(tx.db.QueryRow(ctx, lockTrackedCommitSQL, id))
		if err != nil {
			return fmt.Errorf("lock tracked commit %d: %w", id, err)
		}
		if current.ChecksState != core.ChecksSuccess || !current.Open() {
			commit = current
			return nil
		}
		deploymentID, err := enqueue(tx)
		if err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
		if _, err := tx.db.Exec(ctx, linkTrackedCommitDeploymentSQL, id, deploymentID); err != nil {
			return fmt.Errorf("link deployment %d: %w", deploymentID, err)
		}
		commit, err = tx.GetTrackedCommit(ctx, id)
		promoted = err == nil
		return err
	})
	if err != nil {
		return core.TrackedCommit{}, false, fmt.Errorf("promote tracked commit %d: %w", id, err)
	}
	return commit, promoted, nil
}

func (s *Store) queryTrackedCommits(ctx context.Context, sql string, args ...any) ([]core.TrackedCommit, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.TrackedCommit, error) {
		return scanTrackedCommit(row)
	})
}

func scanTrackedCommit(row pgx.Row) (core.TrackedCommit, error) {
	var (
		c      core.TrackedCommit
		checks []byte
	)
	err := row.Scan(
		&c.ID, &c.Target.App, &c.Target.Environment, &c.Commit.Repository, &c.Commit.SHA, &c.Branch, &c.Source, &c.PushedBy,
		&c.ChecksState, &checks, &c.DeploymentID, &c.SupersededByID,
		&c.CreatedAt, &c.UpdatedAt, &c.EvaluatedAt,
	)
	if err != nil {
		return core.TrackedCommit{}, err
	}
	if err := json.Unmarshal(checks, &c.Checks); err != nil {
		return core.TrackedCommit{}, fmt.Errorf("decode checks of tracked commit %d: %w", c.ID, err)
	}
	return c, nil
}
