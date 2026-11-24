package locks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const SweepInterval = time.Minute

type ReminderOptions struct {
	Repository Repository
	Notifier   Notifier
	Logger     *slog.Logger
	Now        func() time.Time
	Every      time.Duration
}

type Reminder struct {
	repo     Repository
	notifier Notifier
	logger   *slog.Logger
	now      func() time.Time
	every    time.Duration
}

func NewReminder(opts ReminderOptions) (*Reminder, error) {
	if opts.Repository == nil {
		return nil, errors.New("locks: repository is required")
	}
	if opts.Every < 0 {
		return nil, fmt.Errorf("locks: reminder interval must not be negative, got %s", opts.Every)
	}
	r := &Reminder{repo: opts.Repository, notifier: opts.Notifier, logger: opts.Logger, now: opts.Now, every: opts.Every}
	if r.notifier == nil {
		r.notifier = nopNotifier{}
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r, nil
}

func (r *Reminder) RunOnce(ctx context.Context) error {
	expired, expireErr := r.repo.DeleteExpiredFreezes(ctx)
	for _, lock := range expired {
		notifyExpired(ctx, r.notifier, r.logger, lock, r.now())
	}
	if r.every == 0 {
		return expireErr
	}
	due, remindErr := r.repo.ClaimLockReminders(ctx, r.every)
	for _, lock := range due {
		r.logger.InfoContext(ctx, "lock still held",
			slog.String("scope", lock.Scope()),
			slog.String("owner", lock.Owner),
			slog.String("reason", lock.Reason),
			slog.Time("since", lock.CreatedAt),
		)
		notify(ctx, r.notifier, r.logger, core.LockEvent{Action: core.LockActionReminded, Lock: lock, OccurredAt: r.now()})
	}
	return errors.Join(expireErr, remindErr)
}

func (r *Reminder) Run(ctx context.Context, tick time.Duration) error {
	if tick <= 0 {
		return fmt.Errorf("locks: sweep interval must be positive, got %s", tick)
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.logger.ErrorContext(ctx, "lock sweep failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
