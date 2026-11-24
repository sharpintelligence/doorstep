package locks

import (
	"context"
	"encoding/json"
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

	NotifyTimeout = 10 * time.Second
)

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

type Request struct {
	Target    *core.Target
	Reason    string
	ExpiresIn *time.Duration
	ExpiresAt *time.Time
	Actor     core.Actor
	Role      core.Role
}

func (s *Service) Lock(ctx context.Context, req Request) (core.Lock, error) {
	if err := req.Actor.Validate(); err != nil {
		return core.Lock{}, fmt.Errorf("%w: %w", core.ErrInvalidLock, err)
	}
	lockReq := core.LockRequest{
		Target:    req.Target,
		Owner:     req.Actor.String(),
		Reason:    strings.TrimSpace(req.Reason),
		ExpiresIn: req.ExpiresIn,
		ExpiresAt: req.ExpiresAt,
	}
	if err := validate(lockReq); err != nil {
		return core.Lock{}, err
	}
	var lock core.Lock
	var expired []core.Lock
	err := s.repo.InTx(ctx, func(tx Repository) error {
		var err error
		lock, expired, err = tx.AcquireFreeze(ctx, lockReq)
		if err != nil {
			return err
		}
		detail := map[string]any{"scope": lock.Scope(), "reason": lock.Reason}
		if lock.ExpiresAt != nil {
			detail["expires_at"] = lock.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return tx.RecordAudit(ctx, auditEntry(core.AuditLockAcquire, lock, req.Actor, req.Role, detail))
	})
	if err != nil {
		return core.Lock{}, err
	}
	for _, old := range expired {
		notifyExpired(ctx, s.notifier, s.logger, old, s.now())
	}
	s.logger.InfoContext(ctx, "locked",
		slog.String("scope", lock.Scope()),
		slog.String("owner", lock.Owner),
		slog.String("reason", lock.Reason),
	)
	notify(ctx, s.notifier, s.logger, core.LockEvent{Action: core.LockActionLocked, Lock: lock, Actor: lock.Owner, OccurredAt: s.now()})
	return lock, nil
}

func (s *Service) Unlock(ctx context.Context, target *core.Target, actor core.Actor, role core.Role) (core.Lock, error) {
	if err := actor.Validate(); err != nil {
		return core.Lock{}, fmt.Errorf("%w: %w", core.ErrInvalidLock, err)
	}
	actorName := actor.String()
	if err := validateOwner("actor", actorName); err != nil {
		return core.Lock{}, err
	}
	if target != nil {
		if err := target.Validate(); err != nil {
			return core.Lock{}, fmt.Errorf("%w: %w", core.ErrInvalidLock, err)
		}
	}
	var lock core.Lock
	err := s.repo.InTx(ctx, func(tx Repository) error {
		var err error
		lock, err = tx.ReleaseFreeze(ctx, target, actorName)
		if err != nil {
			return err
		}
		detail := map[string]any{
			"scope":     lock.Scope(),
			"owner":     lock.Owner,
			"reason":    lock.Reason,
			"locked_at": lock.CreatedAt.UTC().Format(time.RFC3339),
		}
		return tx.RecordAudit(ctx, auditEntry(core.AuditLockRelease, lock, actor, role, detail))
	})
	if err != nil {
		return core.Lock{}, err
	}
	s.logger.InfoContext(ctx, "unlocked",
		slog.String("scope", lock.Scope()),
		slog.String("owner", lock.Owner),
		slog.String("actor", actorName),
	)
	notify(ctx, s.notifier, s.logger, core.LockEvent{Action: core.LockActionUnlocked, Lock: lock, Actor: actorName, OccurredAt: s.now()})
	return lock, nil
}

func auditEntry(action core.AuditAction, lock core.Lock, actor core.Actor, role core.Role, detail map[string]any) core.AuditEntry {
	data, _ := json.Marshal(detail)
	entry := core.AuditEntry{Actor: actor, Role: role, Action: action, Detail: data}
	if !lock.Global {
		target := lock.Target
		entry.Target = &target
	}
	return entry
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

func notifyExpired(ctx context.Context, n Notifier, logger *slog.Logger, lock core.Lock, now time.Time) {
	logger.InfoContext(ctx, "lock expired",
		slog.String("scope", lock.Scope()),
		slog.String("owner", lock.Owner),
		slog.String("reason", lock.Reason),
	)
	notify(ctx, n, logger, core.LockEvent{Action: core.LockActionExpired, Lock: lock, OccurredAt: now})
}

func notify(ctx context.Context, n Notifier, logger *slog.Logger, ev core.LockEvent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), NotifyTimeout)
	defer cancel()
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
