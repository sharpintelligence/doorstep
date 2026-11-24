package store

import (
	"context"
	"fmt"
	"time"
)

const MaxDeliveryIDLength = 128

const claimWebhookDeliverySQL = `
INSERT INTO webhook_deliveries (delivery_id, event)
VALUES ($1, $2)
ON CONFLICT (delivery_id) DO NOTHING`

const releaseWebhookDeliverySQL = `DELETE FROM webhook_deliveries WHERE delivery_id = $1`

const deleteWebhookDeliveriesBeforeSQL = `DELETE FROM webhook_deliveries WHERE received_at < now() - $1::interval`

func (s *Store) ClaimWebhookDelivery(ctx context.Context, id, event string) (bool, error) {
	if err := validateDeliveryID(id); err != nil {
		return false, fmt.Errorf("claim webhook delivery: %w", err)
	}
	if event == "" {
		return false, fmt.Errorf("claim webhook delivery %s: event must not be empty", id)
	}
	tag, err := s.db.Exec(ctx, claimWebhookDeliverySQL, id, event)
	if err != nil {
		return false, fmt.Errorf("claim webhook delivery %s: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
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
