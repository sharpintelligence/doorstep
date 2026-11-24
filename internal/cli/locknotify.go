package cli

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/notify"
)

type lockNotifier struct {
	notifier notify.Notifier
	apps     []config.App
}

func newLockNotifier(notifier notify.Notifier, catalog config.Catalog) lockNotifier {
	return lockNotifier{notifier: notifier, apps: catalog.Apps}
}

func (n lockNotifier) NotifyLock(ctx context.Context, ev core.LockEvent) error {
	base := core.Notification{Actor: ev.Actor, OccurredAt: ev.OccurredAt}
	switch ev.Action {
	case core.LockActionLocked:
		base.Kind, base.LockChange = core.NotificationLockChanged, core.LockAcquired
	case core.LockActionUnlocked:
		base.Kind, base.LockChange = core.NotificationLockChanged, core.LockReleased
	case core.LockActionExpired:
		base.Kind, base.LockChange, base.Message = core.NotificationLockChanged, core.LockReleased, "the lock expired"
	case core.LockActionReminded:
		base.Kind = core.NotificationLockReminder
	default:
		return nil
	}
	if !ev.Lock.Global {
		n.send(ctx, base, ev.Lock, ev.Lock.Target)
		return nil
	}
	if base.Message == "" {
		base.Message = "global freeze: applies to every app"
	} else {
		base.Message = "global freeze: " + base.Message
	}
	covered := map[string]bool{}
	for _, a := range n.apps {
		if !addsSinks(covered, a.Notify) {
			continue
		}
		n.send(ctx, base, ev.Lock, core.AppTarget(a.Name))
	}
	return nil
}

func (n lockNotifier) send(ctx context.Context, base core.Notification, lock core.Lock, t core.Target) {
	lock.Target = t
	base.Target = t
	base.Lock = &lock
	n.notifier.Notify(ctx, base)
}

func addsSinks(covered map[string]bool, sinks []string) bool {
	added := false
	for _, s := range sinks {
		if !covered[s] {
			covered[s] = true
			added = true
		}
	}
	return added
}
