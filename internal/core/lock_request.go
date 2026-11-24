package core

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrLockHeld    = errors.New("lock is held")
	ErrInvalidLock = errors.New("invalid lock")
)

type LockRequest struct {
	Target    *Target
	Owner     string
	Reason    string
	ExpiresIn *time.Duration
	ExpiresAt *time.Time
}

func (r LockRequest) Scope() string {
	if r.Target == nil {
		return GlobalScope
	}
	return r.Target.App
}

type LockHeldError struct {
	Lock Lock
}

func (e *LockHeldError) Error() string {
	return fmt.Sprintf("%s is already locked by %s: %s", e.Lock.Scope(), e.Lock.Owner, e.Lock.Reason)
}

func (e *LockHeldError) Unwrap() error {
	return ErrLockHeld
}
