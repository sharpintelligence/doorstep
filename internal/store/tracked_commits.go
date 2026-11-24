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
    t.created_at, t.updated_at, t.evaluated_at, t.enqueued_at`

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

const selectOpenTrackedCommitsSQL = `
SELECT` + trackedCommitColumns + `
FROM tracked_commits t
JOIN apps a ON a.id = t.app_id
JOIN unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS f (app, environment, repository, branch)
    ON a.name = f.app AND a.environment = f.environment
   AND lower(t.repository) = lower(f.repository) AND t.branch = f.branch
WHERE t.enqueued_at IS NULL AND t.superseded_by_id IS NULL
ORDER BY t.evaluated_at NULLS FIRST, t.id
LIMIT $5`

const lockAppSQL = `SELECT id FROM apps WHERE id = $1 FOR NO KEY UPDATE`

const insertTrackedCommitSQL = `
INSERT INTO tracked_commits (app_id, repository, branch, commit_sha, source, pushed_by)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (app_id, commit_sha) DO NOTHING
RETURNING id`

const reopenTrackedCommitSQL = `
UPDATE tracked_commits
SET superseded_by_id = NULL
WHERE app_id = $1 AND commit_sha = $2 AND superseded_by_id IS NOT NULL AND enqueued_at IS NULL
RETURNING id`

const selectTrackedCommitIDSQL = `
SELECT id FROM tracked_commits WHERE app_id = $1 AND commit_sha = $2`

const supersedeOtherTrackedCommitsSQL = `
UPDATE tracked_commits
SET superseded_by_id = $2
WHERE app_id = $1
  AND id <> $2
  AND enqueued_at IS NULL
  AND superseded_by_id IS NULL`

const saveCommitChecksSQL = `
UPDATE tracked_commits
SET checks_state = $2, checks = $3, evaluated_at = $4
WHERE id = $1 AND (evaluated_at IS NULL OR evaluated_at < $4)`

const lockTrackedCommitAppSQL = `
SELECT a.id
FROM apps a
JOIN tracked_commits t ON t.app_id = a.id
WHERE t.id = $1
FOR NO KEY UPDATE OF a`

const lockTrackedCommitSQL = selectTrackedCommitSQL + `
FOR NO KEY UPDATE OF t`

const linkTrackedCommitDeploymentSQL = `
UPDATE tracked_commits SET deployment_id = $2, enqueued_at = now() WHERE id = $1`

func (s *Store) TrackCommit(ctx context.Context, c core.TrackedCommit, isHead func(ctx context.Context) (bool, error)) (core.TrackedCommit, core.TrackOutcome, error) {
	if c.ChecksState == "" {
		c.ChecksState = core.ChecksPending
	}
	if c.ChecksState != core.ChecksPending {
		return core.TrackedCommit{}, "", fmt.Errorf("track commit: new commits start %s, got %s", core.ChecksPending, c.ChecksState)
	}
	if c.DeploymentID != nil || c.EnqueuedAt != nil || c.SupersededByID != nil {
		return core.TrackedCommit{}, "", errors.New("track commit: a new commit cannot already be enqueued or superseded")
	}
	if err := c.Validate(); err != nil {
		return core.TrackedCommit{}, "", fmt.Errorf("track commit: %w", err)
	}
	if isHead == nil {
		return core.TrackedCommit{}, "", errors.New("track commit: a branch head check is required")
	}

	var (
		tracked core.TrackedCommit
		outcome core.TrackOutcome
	)
	err := s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.EnsureApp(ctx, c.Target)
		if err != nil {
			return err
		}
		if _, err := tx.db.Exec(ctx, lockAppSQL, appID); err != nil {
			return fmt.Errorf("lock app %s: %w", c.Target, err)
		}
		head, err := isHead(ctx)
		if err != nil {
			return fmt.Errorf("check branch head: %w", err)
		}
		if !head {
			outcome = core.TrackStale
			return nil
		}
		id, trackOutcome, err := tx.insertOrReopen(ctx, appID, c)
		if err != nil {
			return err
		}
		if _, err := tx.db.Exec(ctx, supersedeOtherTrackedCommitsSQL, appID, id); err != nil {
			return fmt.Errorf("supersede commits other than %d: %w", id, err)
		}
		outcome = trackOutcome
		tracked, err = tx.GetTrackedCommit(ctx, id)
		return err
	})
	if err != nil {
		return core.TrackedCommit{}, "", fmt.Errorf("track commit %s for %s: %w", c.Commit.SHA, c.Target, err)
	}
	return tracked, outcome, nil
}

func (s *Store) insertOrReopen(ctx context.Context, appID AppID, c core.TrackedCommit) (core.TrackedCommitID, core.TrackOutcome, error) {
	var id core.TrackedCommitID
	err := s.db.QueryRow(ctx, insertTrackedCommitSQL,
		appID, c.Commit.Repository, c.Branch, c.Commit.SHA, c.Source, c.PushedBy,
	).Scan(&id)
	if err == nil {
		return id, core.TrackCreated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("insert tracked commit: %w", err)
	}
	err = s.db.QueryRow(ctx, reopenTrackedCommitSQL, appID, c.Commit.SHA).Scan(&id)
	if err == nil {
		return id, core.TrackReopened, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", fmt.Errorf("reopen tracked commit: %w", err)
	}
	if err := s.db.QueryRow(ctx, selectTrackedCommitIDSQL, appID, c.Commit.SHA).Scan(&id); err != nil {
		return 0, "", fmt.Errorf("find tracked commit: %w", err)
	}
	return id, core.TrackExisting, nil
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

func (s *Store) OpenTrackedCommits(ctx context.Context, followed []core.FollowedBranch, limit int) ([]core.TrackedCommit, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("open tracked commits: limit must be positive, got %d", limit)
	}
	apps := make([]string, len(followed))
	environments := make([]string, len(followed))
	repositories := make([]string, len(followed))
	branches := make([]string, len(followed))
	for i, f := range followed {
		apps[i], environments[i], repositories[i], branches[i] = f.Target.App, f.Target.Environment, f.Repository, f.Branch
	}
	commits, err := s.queryTrackedCommits(ctx, selectOpenTrackedCommitsSQL, apps, environments, repositories, branches, limit)
	if err != nil {
		return nil, fmt.Errorf("open tracked commits: %w", err)
	}
	return commits, nil
}

func (s *Store) SettleTrackedCommit(ctx context.Context, id core.TrackedCommitID, eval core.ChecksEvaluation, enqueue func(tx *Store) (core.DeploymentID, error)) (core.TrackedCommit, bool, error) {
	if err := eval.Validate(); err != nil {
		return core.TrackedCommit{}, false, fmt.Errorf("settle tracked commit %d: %w", id, err)
	}
	checks := eval.Checks
	if checks == nil {
		checks = []core.Check{}
	}
	encoded, err := json.Marshal(checks)
	if err != nil {
		return core.TrackedCommit{}, false, fmt.Errorf("settle tracked commit %d: encode checks: %w", id, err)
	}

	var (
		commit   core.TrackedCommit
		promoted bool
	)
	err = s.InTx(ctx, func(tx *Store) error {
		current, err := tx.lockTrackedCommit(ctx, id)
		if err != nil || !current.Open() {
			commit = current
			return err
		}
		tag, err := tx.db.Exec(ctx, saveCommitChecksSQL, id, eval.State, encoded, eval.ReadAt)
		if err != nil {
			return fmt.Errorf("save checks: %w", err)
		}
		if tag.RowsAffected() > 0 {
			if current, err = tx.GetTrackedCommit(ctx, id); err != nil {
				return err
			}
		}
		commit = current
		if current.ChecksState != core.ChecksSuccess {
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
		return core.TrackedCommit{}, false, fmt.Errorf("settle tracked commit %d: %w", id, err)
	}
	return commit, promoted, nil
}

func (s *Store) lockTrackedCommit(ctx context.Context, id core.TrackedCommitID) (core.TrackedCommit, error) {
	err := s.db.QueryRow(ctx, lockTrackedCommitAppSQL, id).Scan(new(AppID))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.TrackedCommit{}, fmt.Errorf("tracked commit %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return core.TrackedCommit{}, fmt.Errorf("lock app of tracked commit %d: %w", id, err)
	}
	c, err := scanTrackedCommit(s.db.QueryRow(ctx, lockTrackedCommitSQL, id))
	if err != nil {
		return core.TrackedCommit{}, fmt.Errorf("lock tracked commit %d: %w", id, err)
	}
	return c, nil
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
		&c.CreatedAt, &c.UpdatedAt, &c.EvaluatedAt, &c.EnqueuedAt,
	)
	if err != nil {
		return core.TrackedCommit{}, err
	}
	if err := json.Unmarshal(checks, &c.Checks); err != nil {
		return core.TrackedCommit{}, fmt.Errorf("decode checks of tracked commit %d: %w", c.ID, err)
	}
	return c, nil
}
