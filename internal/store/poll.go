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
SET next_poll_at = now() + $1::interval
WHERE next_poll_at <= now()
RETURNING next_poll_at`

const finishPollSQL = `
UPDATE intake_poll_state
SET next_poll_at = now() + $2::interval
WHERE next_poll_at = $1`

func (s *Store) ClaimPoll(ctx context.Context, lease time.Duration) (time.Time, bool, error) {
	if lease <= 0 {
		return time.Time{}, false, fmt.Errorf("claim poll: lease must be positive, got %s", lease)
	}
	var leasedUntil time.Time
	err := s.db.QueryRow(ctx, claimPollSQL, lease).Scan(&leasedUntil)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("claim poll: %w", err)
	}
	return leasedUntil, true, nil
}

func (s *Store) FinishPoll(ctx context.Context, leasedUntil time.Time, every time.Duration) (bool, error) {
	if every <= 0 {
		return false, fmt.Errorf("finish poll: interval must be positive, got %s", every)
	}
	tag, err := s.db.Exec(ctx, finishPollSQL, leasedUntil, every)
	if err != nil {
		return false, fmt.Errorf("finish poll: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
