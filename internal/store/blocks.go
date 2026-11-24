package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const blockColumns = `
    b.id, a.name, a.environment, COALESCE(b.digest, ''), b.source_repository, COALESCE(b.commit_sha, ''),
    b.reason, b.created_by, b.deployment_id, b.rollback_id, b.created_at,
    COALESCE(b.block_commit_sha, ''), b.cleared_at, COALESCE(b.cleared_by, ''), COALESCE(b.unblock_commit_sha, '')`

const blockTables = `
FROM blocks b
JOIN apps a ON a.id = b.app_id`

const activeBlockByDigestSQL = `SELECT` + blockColumns + blockTables + `
WHERE b.app_id = $1 AND b.cleared_at IS NULL AND b.digest = $2
FOR NO KEY UPDATE OF b`

const activeCommitBlockSQL = `SELECT` + blockColumns + blockTables + `
WHERE b.app_id = $1 AND b.cleared_at IS NULL AND b.commit_sha = $2 AND ($3::boolean OR b.digest IS NULL)
ORDER BY b.digest IS NULL DESC, b.id DESC
LIMIT 1
FOR NO KEY UPDATE OF b`

const insertBlockSQL = `
INSERT INTO blocks (app_id, digest, commit_sha, source_repository, reason, created_by, deployment_id, rollback_id)
VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), $4, $5, $6, $7, $8)
RETURNING id`

const completeBlockSQL = `
UPDATE blocks
SET digest = COALESCE(digest, NULLIF($2, '')),
    commit_sha = COALESCE(commit_sha, NULLIF($3, '')),
    source_repository = CASE WHEN commit_sha IS NULL AND $3 <> '' THEN $4 ELSE source_repository END,
    deployment_id = COALESCE(deployment_id, $5),
    rollback_id = COALESCE(rollback_id, $6)
WHERE id = $1`

const selectBlockSQL = `SELECT` + blockColumns + blockTables + `
WHERE b.id = $1`

const clearBlocksSQL = `
UPDATE blocks b
SET cleared_at = now(), cleared_by = $4
WHERE b.app_id = $1
  AND b.cleared_at IS NULL
  AND (b.digest = NULLIF($2, '') OR b.commit_sha = NULLIF($3, ''))
RETURNING b.id`

const activeBlockSQL = `SELECT` + blockColumns + blockTables + `
WHERE a.name = $1
  AND a.environment = $2
  AND b.cleared_at IS NULL
  AND (b.digest = NULLIF($3, '') OR b.commit_sha = NULLIF($4, ''))
ORDER BY (b.digest = NULLIF($3, '')) IS TRUE DESC, b.id DESC
LIMIT 1`

const listBlocksSQL = `SELECT` + blockColumns + blockTables + `
WHERE a.name = $1
  AND a.environment = $2
  AND ($3::boolean OR b.cleared_at IS NULL)
ORDER BY b.id DESC`

const pendingBlockDigestsSQL = `
SELECT b.digest
FROM blocks b
JOIN apps a ON a.id = b.app_id
WHERE a.name = $1
  AND a.environment = $2
  AND b.cleared_at IS NULL
  AND b.digest IS NOT NULL
  AND b.block_commit_sha IS NULL
ORDER BY b.id`

const pendingUnblockDigestsSQL = `
SELECT b.digest
FROM blocks b
JOIN apps a ON a.id = b.app_id
WHERE a.name = $1
  AND a.environment = $2
  AND b.cleared_at IS NOT NULL
  AND b.digest IS NOT NULL
  AND b.block_commit_sha IS NOT NULL
  AND b.unblock_commit_sha IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM blocks x
      WHERE x.app_id = b.app_id AND x.digest = b.digest AND x.cleared_at IS NULL
  )
GROUP BY b.digest
ORDER BY min(b.id)`

const markBlocksMirroredSQL = `
UPDATE blocks
SET block_commit_sha = $3
WHERE app_id = $1
  AND digest = ANY($2::text[])
  AND block_commit_sha IS NULL`

const markUnblocksMirroredSQL = `
UPDATE blocks
SET unblock_commit_sha = $3
WHERE app_id = $1
  AND digest = ANY($2::text[])
  AND cleared_at IS NOT NULL
  AND block_commit_sha IS NOT NULL
  AND unblock_commit_sha IS NULL`

const markMootUnblocksSQL = `
UPDATE blocks b
SET unblock_commit_sha = $3
WHERE b.app_id = $1
  AND b.digest = ANY($2::text[])
  AND b.cleared_at IS NOT NULL
  AND b.block_commit_sha IS NOT NULL
  AND b.unblock_commit_sha IS NULL
  AND EXISTS (
      SELECT 1 FROM blocks x
      WHERE x.app_id = b.app_id AND x.digest = b.digest AND x.cleared_at IS NULL
  )`

func blockFields(b *core.Block) []any {
	return []any{
		&b.ID, &b.Target.App, &b.Target.Environment, &b.Digest, &b.Commit.Repository, &b.Commit.SHA,
		&b.Reason, &b.CreatedBy, &b.DeploymentID, &b.RollbackID, &b.CreatedAt,
		&b.BlockCommitSHA, &b.ClearedAt, &b.ClearedBy, &b.UnblockCommitSHA,
	}
}

func scanBlock(row pgx.Row) (core.Block, error) {
	var b core.Block
	err := row.Scan(blockFields(&b)...)
	return b, err
}

func (s *Store) queryBlocks(ctx context.Context, sql string, args ...any) ([]core.Block, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (core.Block, error) {
		return scanBlock(row)
	})
}

func (s *Store) LockTarget(ctx context.Context, t core.Target) error {
	appID, err := s.EnsureApp(ctx, t)
	if err != nil {
		return err
	}
	return s.lockApp(ctx, appID)
}

func (s *Store) CreateBlock(ctx context.Context, b core.Block) (core.Block, bool, error) {
	if err := b.Validate(); err != nil {
		return core.Block{}, false, fmt.Errorf("create block: %w", err)
	}
	if b.Commit.SHA == "" {
		b.Commit.Repository = ""
	}
	var (
		result  core.Block
		created bool
	)
	err := s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.EnsureApp(ctx, b.Target)
		if err != nil {
			return err
		}
		if err := tx.lockApp(ctx, appID); err != nil {
			return err
		}
		existing, found, err := tx.findBlockToExtend(ctx, appID, b)
		if err != nil {
			return err
		}
		id := existing.ID
		if found {
			_, err = tx.db.Exec(ctx, completeBlockSQL, id, b.Digest, b.Commit.SHA, b.Commit.Repository, b.DeploymentID, b.RollbackID)
			if err != nil {
				return fmt.Errorf("extend block %d: %w", id, err)
			}
		} else {
			err = tx.db.QueryRow(ctx, insertBlockSQL, appID, b.Digest, b.Commit.SHA, b.Commit.Repository,
				b.Reason, b.CreatedBy, b.DeploymentID, b.RollbackID).Scan(&id)
			if err != nil {
				return fmt.Errorf("insert block: %w", err)
			}
			created = true
		}
		result, err = tx.getBlock(ctx, id)
		return err
	})
	if err != nil {
		return core.Block{}, false, fmt.Errorf("block %s of %s: %w", b.Subject(), b.Target, err)
	}
	return result, created, nil
}

func (s *Store) findBlockToExtend(ctx context.Context, appID AppID, b core.Block) (core.Block, bool, error) {
	if b.Digest != "" {
		found, err := scanBlock(s.db.QueryRow(ctx, activeBlockByDigestSQL, appID, b.Digest))
		if err == nil {
			return found, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return core.Block{}, false, fmt.Errorf("find block of %s: %w", b.Digest, err)
		}
		if b.Commit.SHA == "" {
			return core.Block{}, false, nil
		}
	}
	found, err := scanBlock(s.db.QueryRow(ctx, activeCommitBlockSQL, appID, b.Commit.SHA, b.Digest == ""))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Block{}, false, nil
	}
	if err != nil {
		return core.Block{}, false, fmt.Errorf("find block of commit %s: %w", b.Commit.SHA, err)
	}
	return found, true, nil
}

func (s *Store) getBlock(ctx context.Context, id core.BlockID) (core.Block, error) {
	b, err := scanBlock(s.db.QueryRow(ctx, selectBlockSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Block{}, fmt.Errorf("block %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return core.Block{}, fmt.Errorf("get block %d: %w", id, err)
	}
	return b, nil
}

func (s *Store) ClearBlocks(ctx context.Context, t core.Target, ref core.BlockRef, actor string) ([]core.Block, error) {
	if err := errors.Join(t.Validate(), ref.Validate()); err != nil {
		return nil, fmt.Errorf("unblock: %w", err)
	}
	if strings.TrimSpace(actor) == "" {
		return nil, errors.New("unblock: actor must not be empty")
	}
	var cleared []core.Block
	err := s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.EnsureApp(ctx, t)
		if err != nil {
			return err
		}
		if err := tx.lockApp(ctx, appID); err != nil {
			return err
		}
		rows, err := tx.db.Query(ctx, clearBlocksSQL, appID, ref.Digest, ref.CommitSHA, actor)
		if err != nil {
			return fmt.Errorf("clear blocks: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[core.BlockID])
		if err != nil {
			return fmt.Errorf("clear blocks: %w", err)
		}
		if len(ids) == 0 {
			return core.ErrNotFound
		}
		for _, id := range ids {
			b, err := tx.getBlock(ctx, id)
			if err != nil {
				return err
			}
			cleared = append(cleared, b)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unblock %s of %s: %w", ref, t, err)
	}
	return cleared, nil
}

func (s *Store) ActiveBlock(ctx context.Context, t core.Target, ref core.BlockRef) (core.Block, bool, error) {
	if err := errors.Join(t.Validate(), ref.Validate()); err != nil {
		return core.Block{}, false, fmt.Errorf("find block: %w", err)
	}
	b, err := scanBlock(s.db.QueryRow(ctx, activeBlockSQL, t.App, t.Environment, ref.Digest, ref.CommitSHA))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Block{}, false, nil
	}
	if err != nil {
		return core.Block{}, false, fmt.Errorf("find block of %s for %s: %w", ref, t, err)
	}
	return b, true, nil
}

func (s *Store) Blocked(ctx context.Context, t core.Target, commit core.Commit) (core.Block, bool, error) {
	return s.ActiveBlock(ctx, t, core.BlockRef{CommitSHA: commit.SHA})
}

func (s *Store) ListBlocks(ctx context.Context, t core.Target, includeCleared bool) ([]core.Block, error) {
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("list blocks: %w", err)
	}
	blocks, err := s.queryBlocks(ctx, listBlocksSQL, t.App, t.Environment, includeCleared)
	if err != nil {
		return nil, fmt.Errorf("list blocks of %s: %w", t, err)
	}
	return blocks, nil
}

func (s *Store) PendingBlockTrailers(ctx context.Context, t core.Target) (core.BlockTrailers, error) {
	if err := t.Validate(); err != nil {
		return core.BlockTrailers{}, fmt.Errorf("pending block trailers: %w", err)
	}
	var trailers core.BlockTrailers
	var err error
	trailers.Blocks, err = s.queryDigests(ctx, pendingBlockDigestsSQL, t)
	if err != nil {
		return core.BlockTrailers{}, fmt.Errorf("pending blocks of %s: %w", t, err)
	}
	trailers.Unblocks, err = s.queryDigests(ctx, pendingUnblockDigestsSQL, t)
	if err != nil {
		return core.BlockTrailers{}, fmt.Errorf("pending unblocks of %s: %w", t, err)
	}
	return trailers, nil
}

func (s *Store) queryDigests(ctx context.Context, sql string, t core.Target) ([]core.Digest, error) {
	rows, err := s.db.Query(ctx, sql, t.App, t.Environment)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[core.Digest])
}

func (s *Store) MarkBlocksMirrored(ctx context.Context, t core.Target, trailers core.BlockTrailers, commit core.Commit) error {
	errs := []error{t.Validate(), core.ValidateCommitSHA(commit.SHA)}
	for _, d := range append(append([]core.Digest(nil), trailers.Blocks...), trailers.Unblocks...) {
		errs = append(errs, d.Validate())
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("mark blocks mirrored: %w", err)
	}
	if trailers.IsZero() {
		return nil
	}
	err := s.InTx(ctx, func(tx *Store) error {
		appID, err := tx.EnsureApp(ctx, t)
		if err != nil {
			return err
		}
		if err := tx.lockApp(ctx, appID); err != nil {
			return err
		}
		blocks, unblocks := digestStrings(trailers.Blocks), digestStrings(trailers.Unblocks)
		for _, step := range []struct {
			sql     string
			digests []string
		}{
			{markBlocksMirroredSQL, blocks},
			{markMootUnblocksSQL, blocks},
			{markUnblocksMirroredSQL, unblocks},
		} {
			if len(step.digests) == 0 {
				continue
			}
			if _, err := tx.db.Exec(ctx, step.sql, appID, step.digests, commit.SHA); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("mark blocks of %s mirrored in %s: %w", t, commit.SHA, err)
	}
	return nil
}

func digestStrings(digests []core.Digest) []string {
	out := make([]string, 0, len(digests))
	for _, d := range digests {
		out = append(out, string(d))
	}
	return out
}
