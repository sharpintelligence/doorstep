package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	DeliveryRetention = 7 * 24 * time.Hour
	DeliverySweep     = time.Hour
)

type PostgresDeliveries struct {
	store *store.Store
}

func NewPostgresDeliveries(st *store.Store) *PostgresDeliveries {
	return &PostgresDeliveries{store: st}
}

func (p *PostgresDeliveries) Claim(ctx context.Context, id, event string) (bool, error) {
	return p.store.ClaimWebhookDelivery(ctx, id, event)
}

func (p *PostgresDeliveries) Release(ctx context.Context, id string) error {
	return p.store.ReleaseWebhookDelivery(ctx, id)
}

func (p *PostgresDeliveries) DeleteBefore(ctx context.Context, olderThan time.Duration) (int64, error) {
	return p.store.DeleteWebhookDeliveriesBefore(ctx, olderThan)
}

type ExpiredDeliveries interface {
	DeleteBefore(ctx context.Context, olderThan time.Duration) (int64, error)
}

type SweeperOptions struct {
	Deliveries ExpiredDeliveries
	Logger     *slog.Logger
	Every      time.Duration
	Retention  time.Duration
}

type Sweeper struct {
	deliveries ExpiredDeliveries
	logger     *slog.Logger
	every      time.Duration
	retention  time.Duration
}

func NewSweeper(opts SweeperOptions) (*Sweeper, error) {
	if opts.Deliveries == nil {
		return nil, errors.New("intake: deliveries store is required")
	}
	if opts.Every < 0 || opts.Retention < 0 {
		return nil, fmt.Errorf("intake: sweep interval and retention must not be negative, got %s and %s", opts.Every, opts.Retention)
	}
	s := &Sweeper{deliveries: opts.Deliveries, logger: opts.Logger, every: opts.Every, retention: opts.Retention}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.every == 0 {
		s.every = DeliverySweep
	}
	if s.retention == 0 {
		s.retention = DeliveryRetention
	}
	return s, nil
}

func (s *Sweeper) RunOnce(ctx context.Context) error {
	deleted, err := s.deliveries.DeleteBefore(ctx, s.retention)
	if err != nil {
		return err
	}
	if deleted > 0 {
		s.logger.DebugContext(ctx, "expired webhook deliveries deleted", slog.Int64("deleted", deleted))
	}
	return nil
}

func (s *Sweeper) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger.ErrorContext(ctx, "webhook delivery sweep failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
