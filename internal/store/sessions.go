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

type Session struct {
	ID        int64
	Provider  string
	Subject   string
	Username  string
	Name      string
	Email     string
	Role      core.Role
	CreatedAt time.Time
	ExpiresAt time.Time
}

type NewSession struct {
	Hash     [32]byte
	Provider string
	Subject  string
	Username string
	Name     string
	Email    string
	Role     core.Role
	TTL      time.Duration
}

type Login struct {
	Nonce        string
	PKCEVerifier string
	ReturnTo     string
}

type NewLogin struct {
	StateHash    [32]byte
	Nonce        string
	PKCEVerifier string
	ReturnTo     string
	TTL          time.Duration
	MaxPending   int
}

var ErrTooManyLogins = errors.New("too many sign-ins in progress")

const sessionColumns = `
    id, provider, subject, username, name, email, role, created_at, expires_at`

const insertSessionSQL = `
INSERT INTO auth_sessions (token_hash, provider, subject, username, name, email, role, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now() + $8::bigint * interval '1 microsecond')
RETURNING` + sessionColumns

const findSessionSQL = `
SELECT` + sessionColumns + `
FROM auth_sessions
WHERE token_hash = $1 AND expires_at > now()`

const deleteSessionSQL = `
DELETE FROM auth_sessions
WHERE token_hash = $1
RETURNING` + sessionColumns

const deleteExpiredSessionsSQL = `DELETE FROM auth_sessions WHERE expires_at <= now()`

const deleteExpiredLoginsSQL = `DELETE FROM auth_logins WHERE expires_at <= now()`

const insertLoginSQL = `
INSERT INTO auth_logins (state_hash, nonce, pkce_verifier, return_to, expires_at)
SELECT $1, $2, $3, $4, now() + $5::bigint * interval '1 microsecond'
WHERE $6::bigint <= 0
   OR (SELECT count(*) FROM (SELECT 1 FROM auth_logins WHERE expires_at > now() LIMIT $6::bigint) AS pending) < $6::bigint`

const listSessionsSQL = `
SELECT` + sessionColumns + `
FROM auth_sessions
WHERE expires_at > now()
ORDER BY created_at, id`

const deleteUserSessionsSQL = `
DELETE FROM auth_sessions
WHERE subject = $1 OR provider || ':' || subject = $1 OR lower(username) = lower($1)`

const deleteAllSessionsSQL = `DELETE FROM auth_sessions`

const consumeLoginSQL = `
DELETE FROM auth_logins
WHERE state_hash = $1
RETURNING nonce, pkce_verifier, return_to, expires_at > now()`

func (s *Store) CreateSession(ctx context.Context, n NewSession) (Session, error) {
	if !n.Role.Valid() {
		return Session{}, fmt.Errorf("create session: %w %q", core.ErrUnknownRole, n.Role)
	}
	if n.TTL <= 0 {
		return Session{}, errors.New("create session: ttl must be positive")
	}
	session, err := scanSession(s.db.QueryRow(ctx, insertSessionSQL,
		n.Hash[:], n.Provider, n.Subject, n.Username, n.Name, n.Email, n.Role, n.TTL.Microseconds()))
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return session, nil
}

func (s *Store) FindSessionByHash(ctx context.Context, hash [32]byte) (Session, error) {
	session, err := scanSession(s.db.QueryRow(ctx, findSessionSQL, hash[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, fmt.Errorf("session: %w", core.ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("find session: %w", err)
	}
	return session, nil
}

func (s *Store) DeleteSessionByHash(ctx context.Context, hash [32]byte) (Session, error) {
	session, err := scanSession(s.db.QueryRow(ctx, deleteSessionSQL, hash[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, fmt.Errorf("session: %w", core.ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("delete session: %w", err)
	}
	return session, nil
}

func (s *Store) DeleteExpiredAuthRows(ctx context.Context) (int64, error) {
	sessions, err := s.db.Exec(ctx, deleteExpiredSessionsSQL)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	logins, err := s.db.Exec(ctx, deleteExpiredLoginsSQL)
	if err != nil {
		return 0, fmt.Errorf("delete expired logins: %w", err)
	}
	return sessions.RowsAffected() + logins.RowsAffected(), nil
}

func (s *Store) CreateLogin(ctx context.Context, n NewLogin) error {
	if n.TTL <= 0 {
		return errors.New("create login: ttl must be positive")
	}
	tag, err := s.db.Exec(ctx, insertLoginSQL, n.StateHash[:], n.Nonce, n.PKCEVerifier, n.ReturnTo, n.TTL.Microseconds(), n.MaxPending)
	if err != nil {
		return fmt.Errorf("create login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("create login: %w", ErrTooManyLogins)
	}
	return nil
}

func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.Query(ctx, listSessionsSQL)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	sessions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) { return scanSession(row) })
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return sessions, nil
}

func (s *Store) DeleteUserSessions(ctx context.Context, user string) (int64, error) {
	if strings.TrimSpace(user) == "" {
		return 0, errors.New("delete user sessions: user must not be empty")
	}
	tag, err := s.db.Exec(ctx, deleteUserSessionsSQL, user)
	if err != nil {
		return 0, fmt.Errorf("delete user sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) DeleteAllSessions(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx, deleteAllSessionsSQL)
	if err != nil {
		return 0, fmt.Errorf("delete all sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) ConsumeLogin(ctx context.Context, stateHash [32]byte) (Login, error) {
	var l Login
	var live bool
	err := s.db.QueryRow(ctx, consumeLoginSQL, stateHash[:]).Scan(&l.Nonce, &l.PKCEVerifier, &l.ReturnTo, &live)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return Login{}, fmt.Errorf("login: %w", core.ErrNotFound)
	}
	if err != nil {
		return Login{}, fmt.Errorf("consume login: %w", err)
	}
	return l, nil
}

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.Provider, &s.Subject, &s.Username, &s.Name, &s.Email, &s.Role, &s.CreatedAt, &s.ExpiresAt)
	return s, err
}
