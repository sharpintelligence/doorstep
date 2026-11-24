package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const MaxDeliveryIDLength = 128

type DeliveryClaim string

const (
	DeliveryClaimed    DeliveryClaim = "claimed"
	DeliveryCompleted  DeliveryClaim = "completed"
	DeliveryInProgress DeliveryClaim = "in_progress"
)

const claimWebhookDeliverySQL = `
INSERT INTO webhook_deliveries (delivery_id, event)
VALUES ($1, $2)
ON CONFLICT (delivery_id) DO UPDATE
SET event = EXCLUDED.event, claimed_at = now()
WHERE webhook_deliveries.completed_at IS NULL
  AND webhook_deliveries.claimed_at <= now() - $3::interval`

const webhookDeliveryCompletedSQL = `SELECT completed_at IS NOT NULL FROM webhook_deliveries WHERE delivery_id = $1`

const completeWebhookDeliverySQL = `
UPDATE webhook_deliveries
SET completed_at = greatest(now(), claimed_at)
WHERE delivery_id = $1 AND completed_at IS NULL`

const releaseWebhookDeliverySQL = `DELETE FROM webhook_deliveries WHERE delivery_id = $1 AND completed_at IS NULL`

const deleteWebhookDeliveriesBeforeSQL = `DELETE FROM webhook_deliveries WHERE claimed_at < now() - $1::interval`

func (s *Store) ClaimWebhookDelivery(ctx context.Context, id, event string, staleAfter time.Duration) (DeliveryClaim, error) {
	if err := validateDeliveryID(id); err != nil {
		return "", fmt.Errorf("claim webhook delivery: %w", err)
	}
	if event == "" {
		return "", fmt.Errorf("claim webhook delivery %s: event must not be empty", id)
	}
	if staleAfter <= 0 {
		return "", fmt.Errorf("claim webhook delivery %s: stale age must be positive, got %s", id, staleAfter)
	}
	tag, err := s.db.Exec(ctx, claimWebhookDeliverySQL, id, event, staleAfter)
	if err != nil {
		return "", fmt.Errorf("claim webhook delivery %s: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return DeliveryClaimed, nil
	}
	var completed bool
	err = s.db.QueryRow(ctx, webhookDeliveryCompletedSQL, id).Scan(&completed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return DeliveryInProgress, nil
	case err != nil:
		return "", fmt.Errorf("claim webhook delivery %s: %w", id, err)
	case completed:
		return DeliveryCompleted, nil
	default:
		return DeliveryInProgress, nil
	}
}

func (s *Store) CompleteWebhookDelivery(ctx context.Context, id string) error {
	if _, err := s.db.Exec(ctx, completeWebhookDeliverySQL, id); err != nil {
		return fmt.Errorf("complete webhook delivery %s: %w", id, err)
	}
	return nil
}

func (s *Store) ReleaseWebhookDelivery(ctx context.Context, id string) error {
	if _, err := s.db.Exec(ctx, releaseWebhookDeliverySQL, id); err != nil {
		return fmt.Errorf("release webhook delivery %s: %w", id, err)
	}
	return nil
}

func (s *Store) DeleteWebhookDeliveriesBefore(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("delete webhook deliveries: age must be positive, got %s", olderThan)
	}
	tag, err := s.db.Exec(ctx, deleteWebhookDeliveriesBeforeSQL, olderThan)
	if err != nil {
		return 0, fmt.Errorf("delete webhook deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

func validateDeliveryID(id string) error {
	if id == "" || len(id) > MaxDeliveryIDLength {
		return fmt.Errorf("delivery id must be 1 to %d characters, got %d", MaxDeliveryIDLength, len(id))
	}
	return nil
}
