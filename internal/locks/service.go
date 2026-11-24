package locks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	MaxOwnerLength  = 200
	MaxReasonLength = 500
)

type Repository interface {
	AcquireFreeze(ctx context.Context, req core.LockRequest) (core.Lock, error)
	ReleaseFreeze(ctx context.Context, target *core.Target, actor string) (core.Lock, error)
	ListFreezes(ctx context.Context) ([]core.Lock, error)
	BlockingFreezes(ctx context.Context, t core.Target) ([]core.Lock, error)
	DeleteExpiredFreezes(ctx context.Context) ([]core.Lock, error)
	ClaimLockReminders(ctx context.Context, every time.Duration) ([]core.Lock, error)
}

type Notifier interface {
	NotifyLock(ctx context.Context, ev core.LockEvent) error
}

type Options struct {
	Repository Repository
	Notifier   Notifier
	Logger     *slog.Logger
	Now        func() time.Time
}

type Service struct {
	repo     Repository
	notifier Notifier
	logger   *slog.Logger
	now      func() time.Time
}

func New(opts Options) (*Service, error) {
	if opts.Repository == nil {
		return nil, errors.New("locks: repository is required")
	}
	s := &Service{repo: opts.Repository, notifier: opts.Notifier, logger: opts.Logger, now: opts.Now}
	if s.notifier == nil {
		s.notifier = nopNotifier{}
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

func (s *Service) Lock(ctx context.Context, req core.LockRequest) (core.Lock, error) {
	req.Owner = strings.TrimSpace(req.Owner)
	req.Reason = strings.TrimSpace(req.Reason)
	if err := validate(req); err != nil {
		return core.Lock{}, err
	}
	lock, err := s.repo.AcquireFreeze(ctx, req)
	if err != nil {
		return core.Lock{}, err
	}
	s.logger.InfoContext(ctx, "locked",
		slog.String("scope", lock.Scope()),
		slog.String("owner", lock.Owner),
		slog.String("reason", lock.Reason),
	)
	notify(ctx, s.notifier, s.logger, core.LockEvent{Action: core.LockActionLocked, Lock: lock, Actor: lock.Owner, OccurredAt: s.now()})
	return lock, nil
}

func (s *Service) Unlock(ctx context.Context, target *core.Target, actor string) (core.Lock, error) {
	actor = strings.TrimSpace(actor)
	if err := validateOwner("actor", actor); err != nil {
		return core.Lock{}, err
	}
	if target != nil {
		if err := target.Validate(); err != nil {
			return core.Lock{}, fmt.Errorf("%w: %w", core.ErrInvalidLock, err)
		}
	}
	lock, err := s.repo.ReleaseFreeze(ctx, target, actor)
	if err != nil {
		return core.Lock{}, err
	}
	s.logger.InfoContext(ctx, "unlocked",
		slog.String("scope", lock.Scope()),
		slog.String("owner", lock.Owner),
		slog.String("actor", actor),
	)
	notify(ctx, s.notifier, s.logger, core.LockEvent{Action: core.LockActionUnlocked, Lock: lock, Actor: actor, OccurredAt: s.now()})
	return lock, nil
}

func (s *Service) List(ctx context.Context) ([]core.Lock, error) {
	return s.repo.ListFreezes(ctx)
}

func (s *Service) Blocking(ctx context.Context, t core.Target) ([]core.Lock, error) {
	return s.repo.BlockingFreezes(ctx, t)
}

func validate(req core.LockRequest) error {
	var errs []error
	if req.Target != nil {
		if err := req.Target.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%w: %w", core.ErrInvalidLock, err))
		}
	}
	errs = append(errs, validateOwner("owner", req.Owner))
	switch n := utf8.RuneCountInString(req.Reason); {
	case !utf8.ValidString(req.Reason):
		errs = append(errs, fmt.Errorf("%w: reason must be valid UTF-8", core.ErrInvalidLock))
	case n == 0:
		errs = append(errs, fmt.Errorf("%w: reason is required", core.ErrInvalidLock))
	case n > MaxReasonLength:
		errs = append(errs, fmt.Errorf("%w: reason must be at most %d characters, got %d", core.ErrInvalidLock, MaxReasonLength, n))
	case strings.ContainsFunc(req.Reason, unicode.IsControl):
		errs = append(errs, fmt.Errorf("%w: reason must not contain control characters", core.ErrInvalidLock))
	}
	if req.ExpiresIn != nil && req.ExpiresAt != nil {
		errs = append(errs, fmt.Errorf("%w: set at most one of an expiry duration and an expiry time", core.ErrInvalidLock))
	}
	if req.ExpiresIn != nil && *req.ExpiresIn <= 0 {
		errs = append(errs, fmt.Errorf("%w: expiry duration must be positive, got %s", core.ErrInvalidLock, *req.ExpiresIn))
	}
	return errors.Join(errs...)
}

func validateOwner(field, owner string) error {
	switch n := utf8.RuneCountInString(owner); {
	case n == 0:
		return fmt.Errorf("%w: %s is required", core.ErrInvalidLock, field)
	case n > MaxOwnerLength:
		return fmt.Errorf("%w: %s must be at most %d characters, got %d", core.ErrInvalidLock, field, MaxOwnerLength, n)
	case !utf8.ValidString(owner) || strings.ContainsFunc(owner, unicode.IsControl):
		return fmt.Errorf("%w: %s must be valid UTF-8 without control characters", core.ErrInvalidLock, field)
	}
	return nil
}

func notify(ctx context.Context, n Notifier, logger *slog.Logger, ev core.LockEvent) {
	if err := n.NotifyLock(ctx, ev); err != nil {
		logger.WarnContext(ctx, "lock notification failed",
			slog.String("action", string(ev.Action)),
			slog.String("scope", ev.Lock.Scope()),
			slog.Any("error", err),
		)
	}
}

type nopNotifier struct{}

func (nopNotifier) NotifyLock(context.Context, core.LockEvent) error { return nil }
