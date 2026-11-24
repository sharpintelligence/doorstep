package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	uniqueViolation         = "23505"
	apiAccessNameConstraint = "api_tokens_name_key"
	apiTokenTouchInterval   = "1 minute"
)

var ErrTokenExists = errors.New("an api token with this name already exists")

type APIToken struct {
	ID         int64
	Name       string
	Role       core.Role
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	Expired    bool
}

type NewAPIToken struct {
	Name      string
	Hash      [32]byte
	Role      core.Role
	CreatedBy string
	TTL       time.Duration
}

const apiAccessColumns = `
    id, name, role, created_by, created_at, expires_at, last_used_at, revoked_at,
    expires_at IS NOT NULL AND expires_at <= now()`

const insertAPITokenSQL = `
INSERT INTO api_tokens (name, token_hash, role, created_by, expires_at)
VALUES ($1, $2, $3, $4, CASE WHEN $5::bigint > 0 THEN now() + $5::bigint * interval '1 microsecond' END)
RETURNING` + apiAccessColumns

const findAPITokenSQL = `
SELECT` + apiAccessColumns + `
FROM api_tokens
WHERE token_hash = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now())`

const touchAPITokenSQL = `
UPDATE api_tokens SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '` + apiTokenTouchInterval + `')`

const listAPITokensSQL = `
SELECT` + apiAccessColumns + `
FROM api_tokens
ORDER BY name`

const revokeAPITokenSQL = `
UPDATE api_tokens SET revoked_at = now()
WHERE name = $1 AND revoked_at IS NULL
RETURNING` + apiAccessColumns

func (s *Store) CreateAPIToken(ctx context.Context, t NewAPIToken) (APIToken, error) {
	if !t.Role.Valid() {
		return APIToken{}, fmt.Errorf("create api token %s: %w %q", t.Name, core.ErrUnknownRole, t.Role)
	}
	if t.TTL < 0 {
		return APIToken{}, fmt.Errorf("create api token %s: ttl must not be negative", t.Name)
	}
	created, err := scanAPIToken(s.db.QueryRow(ctx, insertAPITokenSQL,
		t.Name, t.Hash[:], t.Role, t.CreatedBy, t.TTL.Microseconds()))
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == uniqueViolation && pgErr.ConstraintName == apiAccessNameConstraint {
		return APIToken{}, fmt.Errorf("create api token %s: %w", t.Name, ErrTokenExists)
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("create api token %s: %w", t.Name, err)
	}
	return created, nil
}

func (s *Store) FindAPITokenByHash(ctx context.Context, hash [32]byte) (APIToken, error) {
	t, err := scanAPIToken(s.db.QueryRow(ctx, findAPITokenSQL, hash[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIToken{}, fmt.Errorf("api token: %w", core.ErrNotFound)
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("find api token: %w", err)
	}
	return t, nil
}

func (s *Store) TouchAPIToken(ctx context.Context, id int64) error {
	if _, err := s.db.Exec(ctx, touchAPITokenSQL, id); err != nil {
		return fmt.Errorf("touch api token %d: %w", id, err)
	}
	return nil
}

func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.Query(ctx, listAPITokensSQL)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	tokens, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (APIToken, error) {
		return scanAPIToken(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	return tokens, nil
}

func (s *Store) RevokeAPIToken(ctx context.Context, name string) (APIToken, error) {
	t, err := scanAPIToken(s.db.QueryRow(ctx, revokeAPITokenSQL, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return APIToken{}, fmt.Errorf("api token %s: %w", name, core.ErrNotFound)
	}
	if err != nil {
		return APIToken{}, fmt.Errorf("revoke api token %s: %w", name, err)
	}
	return t, nil
}

func scanAPIToken(row pgx.Row) (APIToken, error) {
	var t APIToken
	err := row.Scan(&t.ID, &t.Name, &t.Role, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt, &t.Expired)
	return t, err
}
