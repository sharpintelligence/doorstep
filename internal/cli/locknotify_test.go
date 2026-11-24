package cli

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

type recordingNotifier struct {
	mu   sync.Mutex
	sent []core.Notification
}

func (r *recordingNotifier) Notify(_ context.Context, n core.Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
}

func TestLockNotifier(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	appLock := core.Lock{Target: core.AppTarget("blog"), Kind: core.LockFreeze, Owner: "user:alice", Reason: "incident"}
	globalLock := core.Lock{Global: true, Kind: core.LockFreeze, Owner: "user:alice", Reason: "holidays"}
	apps := []config.App{
		{Name: "blog", Notify: []string{"ops"}},
		{Name: "shop", Notify: []string{"ops"}},
		{Name: "api", Notify: []string{"ops", "hooks"}},
		{Name: "quiet"},
	}
	type sent struct {
		kind    core.NotificationKind
		change  core.LockChange
		app     string
		message string
	}
	tests := []struct {
		name string
		ev   core.LockEvent
		want []sent
	}{
		{
			name: "app locked",
			ev:   core.LockEvent{Action: core.LockActionLocked, Lock: appLock, Actor: "user:alice", OccurredAt: at},
			want: []sent{{kind: core.NotificationLockChanged, change: core.LockAcquired, app: "blog"}},
		},
		{
			name: "app unlocked",
			ev:   core.LockEvent{Action: core.LockActionUnlocked, Lock: appLock, Actor: "user:bob", OccurredAt: at},
			want: []sent{{kind: core.NotificationLockChanged, change: core.LockReleased, app: "blog"}},
		},
		{
			name: "app lock expired",
			ev:   core.LockEvent{Action: core.LockActionExpired, Lock: appLock, Actor: "doorstep", OccurredAt: at},
			want: []sent{{kind: core.NotificationLockChanged, change: core.LockReleased, app: "blog", message: "the lock expired"}},
		},
		{
			name: "app lock reminder",
			ev:   core.LockEvent{Action: core.LockActionReminded, Lock: appLock, Actor: "doorstep", OccurredAt: at},
			want: []sent{{kind: core.NotificationLockReminder, app: "blog"}},
		},
		{
			name: "global freeze fans out once per sink",
			ev:   core.LockEvent{Action: core.LockActionLocked, Lock: globalLock, Actor: "user:alice", OccurredAt: at},
			want: []sent{
				{kind: core.NotificationLockChanged, change: core.LockAcquired, app: "blog", message: "global freeze: applies to every app"},
				{kind: core.NotificationLockChanged, change: core.LockAcquired, app: "api", message: "global freeze: applies to every app"},
			},
		},
		{
			name: "global freeze expired",
			ev:   core.LockEvent{Action: core.LockActionExpired, Lock: globalLock, Actor: "doorstep", OccurredAt: at},
			want: []sent{
				{kind: core.NotificationLockChanged, change: core.LockReleased, app: "blog", message: "global freeze: the lock expired"},
				{kind: core.NotificationLockChanged, change: core.LockReleased, app: "api", message: "global freeze: the lock expired"},
			},
		},
		{
			name: "unknown action",
			ev:   core.LockEvent{Action: "renamed", Lock: appLock, OccurredAt: at},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recordingNotifier{}
			n := newLockNotifier(rec, config.Catalog{Apps: apps})
			if err := n.NotifyLock(t.Context(), tt.ev); err != nil {
				t.Fatalf("NotifyLock: %v", err)
			}
			if len(rec.sent) != len(tt.want) {
				t.Fatalf("sent %d notifications, want %d: %+v", len(rec.sent), len(tt.want), rec.sent)
			}
			for i, w := range tt.want {
				got := rec.sent[i]
				target := core.AppTarget(w.app)
				if got.Kind != w.kind || got.LockChange != w.change || got.Target != target || got.Message != w.message {
					t.Errorf("notification %d = %+v, want %+v", i, got, w)
				}
				if got.Lock == nil || got.Lock.Target != target || got.Lock.Owner != tt.ev.Lock.Owner || got.Lock.Global != tt.ev.Lock.Global {
					t.Errorf("notification %d lock = %+v", i, got.Lock)
				}
				if got.Actor != tt.ev.Actor || !got.OccurredAt.Equal(at) {
					t.Errorf("notification %d actor/time = %q %s", i, got.Actor, got.OccurredAt)
				}
				if err := got.Validate(); err != nil {
					t.Errorf("notification %d does not validate: %v", i, err)
				}
			}
		})
	}
}
