package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const ensureAppAttempts = 3

type AppID int64

const ensureAppSQL = `
WITH inserted AS (
    INSERT INTO apps (name, environment) VALUES ($1, $2)
    ON CONFLICT (name, environment) DO NOTHING
    RETURNING id
)
SELECT id FROM inserted
UNION ALL
SELECT id FROM apps WHERE name = $1 AND environment = $2
LIMIT 1`

func (s *Store) EnsureApp(ctx context.Context, t core.Target) (AppID, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	for range ensureAppAttempts {
		var id AppID
		err := s.db.QueryRow(ctx, ensureAppSQL, t.App, t.Environment).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("ensure app %s: %w", t, err)
		}
	}
	return 0, fmt.Errorf("ensure app %s: row not visible after %d attempts", t, ensureAppAttempts)
}
