package core

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

var ErrUnknownNotificationKind = errors.New("unknown notification kind")

type NotificationKind string

const (
	NotificationDeployQueued     NotificationKind = "deploy.queued"
	NotificationDeployStarted    NotificationKind = "deploy.started"
	NotificationDeploySucceeded  NotificationKind = "deploy.succeeded"
	NotificationDeployFailed     NotificationKind = "deploy.failed"
	NotificationDeployRolledBack NotificationKind = "deploy.rolled_back"
	NotificationDeployCancelled  NotificationKind = "deploy.cancelled"
	NotificationDeploySuperseded NotificationKind = "deploy.superseded"
	NotificationLockChanged      NotificationKind = "lock.changed"
	NotificationLockReminder     NotificationKind = "lock.reminder"
)

func NotificationKinds() []NotificationKind {
	return []NotificationKind{
		NotificationDeployQueued,
		NotificationDeployStarted,
		NotificationDeploySucceeded,
		NotificationDeployFailed,
		NotificationDeployRolledBack,
		NotificationDeployCancelled,
		NotificationDeploySuperseded,
		NotificationLockChanged,
		NotificationLockReminder,
	}
}

func DefaultNotificationKinds() []NotificationKind {
	return []NotificationKind{
		NotificationDeployStarted,
		NotificationDeploySucceeded,
		NotificationDeployFailed,
		NotificationDeployRolledBack,
		NotificationLockChanged,
		NotificationLockReminder,
	}
}

func ParseNotificationKind(s string) (NotificationKind, error) {
	kind := NotificationKind(s)
	if !kind.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownNotificationKind, s)
	}
	return kind, nil
}

func (k NotificationKind) String() string {
	return string(k)
}

func (k NotificationKind) Valid() bool {
	return slices.Contains(NotificationKinds(), k)
}

func (k NotificationKind) IsDeploy() bool {
	return k.Valid() && !k.IsLock()
}

func (k NotificationKind) IsLock() bool {
	return k == NotificationLockChanged || k == NotificationLockReminder
}

type LockChange string

const (
	LockAcquired LockChange = "acquired"
	LockReleased LockChange = "released"
)

func LockChanges() []LockChange {
	return []LockChange{LockAcquired, LockReleased}
}

func (c LockChange) Valid() bool {
	return slices.Contains(LockChanges(), c)
}

type Notification struct {
	Kind       NotificationKind
	Target     Target
	Deployment *Deployment
	RollbackOf *Deployment
	Lock       *Lock
	LockChange LockChange
	Actor      string
	Message    string
	OccurredAt time.Time
}

func (n Notification) Validate() error {
	if !n.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownNotificationKind, n.Kind)
	}
	errs := []error{n.Target.Validate()}
	if n.Kind.IsDeploy() {
		errs = append(errs, n.validateDeployment()...)
	} else {
		errs = append(errs, n.validateLock()...)
	}
	return errors.Join(errs...)
}

func (n Notification) validateDeployment() []error {
	var errs []error
	if n.Lock != nil || n.LockChange != "" {
		errs = append(errs, fmt.Errorf("%s notification must not carry a lock", n.Kind))
	}
	if n.Deployment == nil {
		return append(errs, fmt.Errorf("%s notification needs a deployment", n.Kind))
	}
	if n.Deployment.Target != n.Target {
		errs = append(errs, fmt.Errorf("deployment target %s does not match notification target %s", n.Deployment.Target, n.Target))
	}
	if n.Kind == NotificationDeployRolledBack {
		if n.Deployment.Kind != KindRollback {
			errs = append(errs, fmt.Errorf("%s notification needs a deployment of kind %s", n.Kind, KindRollback))
		}
		if n.RollbackOf == nil {
			errs = append(errs, fmt.Errorf("%s notification needs the deployment that was rolled back", n.Kind))
		}
	}
	if n.RollbackOf != nil {
		if n.Deployment.Kind != KindRollback {
			errs = append(errs, errors.New("only a rollback deployment may name the deployment it rolls back"))
		}
		if n.RollbackOf.Target != n.Target {
			errs = append(errs, fmt.Errorf("rolled back deployment target %s does not match notification target %s", n.RollbackOf.Target, n.Target))
		}
	}
	return errs
}

func (n Notification) validateLock() []error {
	var errs []error
	if n.Deployment != nil || n.RollbackOf != nil {
		errs = append(errs, fmt.Errorf("%s notification must not carry a deployment", n.Kind))
	}
	if n.Lock == nil {
		errs = append(errs, fmt.Errorf("%s notification needs a lock", n.Kind))
	} else if n.Lock.Target != n.Target {
		errs = append(errs, fmt.Errorf("lock target %s does not match notification target %s", n.Lock.Target, n.Target))
	}
	switch {
	case n.Kind == NotificationLockChanged && !n.LockChange.Valid():
		errs = append(errs, fmt.Errorf("%s notification needs a lock change of acquired or released, got %q", n.Kind, n.LockChange))
	case n.Kind == NotificationLockReminder && n.LockChange != "":
		errs = append(errs, fmt.Errorf("%s notification must not carry a lock change", n.Kind))
	}
	return errs
}
