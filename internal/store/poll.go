package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const claimPollSQL = `
UPDATE intake_poll_state
SET polled_at = now()
WHERE polled_at <= now() - $1::interval
RETURNING polled_at`

func (s *Store) ClaimPoll(ctx context.Context, every time.Duration) (bool, error) {
	if every <= 0 {
		return false, fmt.Errorf("claim poll: interval must be positive, got %s", every)
	}
	var polledAt time.Time
	err := s.db.QueryRow(ctx, claimPollSQL, every).Scan(&polledAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("claim poll: %w", err)
	}
	return true, nil
}
