package locks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

var (
	alice     = core.Actor{Kind: core.ActorUser, ID: "alice"}
	bob       = core.Actor{Kind: core.ActorUser, ID: "bob"}
	errBoom   = errors.New("boom")
	fixedTime = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	web       = core.Target{App: "web", Environment: core.DefaultEnvironment}
)

type fakeRepo struct {
	mu sync.Mutex

	acquired  []core.LockRequest
	audits    []core.AuditEntry
	auditErr  error
	commits   int
	rollbacks int
	released  []string
	remindFor []time.Duration
	sweeps    int

	acquireErr error
	releaseErr error
	listErr    error
	expireErr  error
	remindErr  error

	listed   []core.Lock
	blocking []core.Lock
	expired  []core.Lock
	purged   []core.Lock
	due      []core.Lock
	blockArg core.Target
}

func (f *fakeRepo) AcquireFreeze(_ context.Context, req core.LockRequest) (core.Lock, []core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, req)
	if f.acquireErr != nil {
		return core.Lock{}, nil, f.acquireErr
	}
	l := core.Lock{Global: req.Target == nil, Kind: core.LockFreeze, Owner: req.Owner, Reason: req.Reason, CreatedAt: fixedTime}
	if req.Target != nil {
		l.Target = *req.Target
	}
	return l, f.purged, nil
}

func (f *fakeRepo) ReleaseFreeze(_ context.Context, target *core.Target, actor string) (core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	scope := core.LockRequest{Target: target}.Scope()
	f.released = append(f.released, scope+" by "+actor)
	if f.releaseErr != nil {
		return core.Lock{}, f.releaseErr
	}
	l := core.Lock{Global: target == nil, Kind: core.LockFreeze, Owner: "alice", Reason: "held"}
	if target != nil {
		l.Target = *target
	}
	return l, nil
}

func (f *fakeRepo) RecordAudit(_ context.Context, e core.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

func (f *fakeRepo) InTx(_ context.Context, fn func(Repository) error) error {
	snapshot := f.audits
	err := fn(f)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.audits = snapshot
		f.rollbacks++
		return err
	}
	f.commits++
	return nil
}

func (f *fakeRepo) ListFreezes(context.Context) ([]core.Lock, error) {
	return f.listed, f.listErr
}

func (f *fakeRepo) BlockingFreezes(_ context.Context, t core.Target) ([]core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockArg = t
	return f.blocking, f.listErr
}

func (f *fakeRepo) DeleteExpiredFreezes(context.Context) ([]core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps++
	return f.expired, f.expireErr
}

func (f *fakeRepo) ClaimLockReminders(_ context.Context, every time.Duration) ([]core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remindFor = append(f.remindFor, every)
	return f.due, f.remindErr
}

func (f *fakeRepo) sweepCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sweeps
}

type recordingNotifier struct {
	mu     sync.Mutex
	events []core.LockEvent
	err    error
}

func (n *recordingNotifier) NotifyLock(_ context.Context, ev core.LockEvent) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
	return n.err
}

func (n *recordingNotifier) summary() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.events))
	for i, ev := range n.events {
		out[i] = string(ev.Action) + " " + ev.Lock.Scope() + " actor=" + ev.Actor
	}
	return out
}

func durationPtr(d time.Duration) *time.Duration { return &d }

func timePtr(t time.Time) *time.Time { return &t }

func newService(t *testing.T, repo *fakeRepo, n Notifier, logs *bytes.Buffer) *Service {
	t.Helper()
	var logger *slog.Logger
	if logs != nil {
		logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	s, err := New(Options{Repository: repo, Notifier: n, Logger: logger, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNewRequiresRepository(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{}); err == nil {
		t.Error("New without a repository succeeded")
	}
	if _, err := NewReminder(ReminderOptions{}); err == nil {
		t.Error("NewReminder without a repository succeeded")
	}
	if _, err := NewReminder(ReminderOptions{Repository: &fakeRepo{}, Every: -time.Second}); err == nil {
		t.Error("NewReminder with a negative interval succeeded")
	}
	s, err := New(Options{Repository: &fakeRepo{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Lock(t.Context(), Request{Actor: alice, Reason: "r"}); err != nil {
		t.Errorf("Lock with default notifier and logger: %v", err)
	}
}

func TestLockValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		req        Request
		wantErr    []string
		wantOwner  string
		wantReason string
	}{
		{name: "app", req: Request{Target: &web, Actor: alice, Reason: "migration"}, wantOwner: "user:alice", wantReason: "migration"},
		{name: "global", req: Request{Actor: alice, Reason: "holiday"}, wantOwner: "user:alice", wantReason: "holiday"},
		{name: "trimmed", req: Request{Actor: alice, Reason: "\n migration  "}, wantOwner: "user:alice", wantReason: "migration"},
		{name: "expires in", req: Request{Actor: alice, Reason: "r", ExpiresIn: durationPtr(time.Hour)}, wantOwner: "user:alice", wantReason: "r"},
		{name: "expires at", req: Request{Actor: alice, Reason: "r", ExpiresAt: timePtr(fixedTime.Add(time.Hour))}, wantOwner: "user:alice", wantReason: "r"},
		{name: "unicode reason at limit", req: Request{Actor: alice, Reason: strings.Repeat("é", MaxReasonLength)}, wantOwner: "user:alice", wantReason: strings.Repeat("é", MaxReasonLength)},
		{name: "owner at limit", req: Request{Actor: core.Actor{Kind: core.ActorUser, ID: strings.Repeat("a", MaxOwnerLength-len("user:"))}, Reason: "r"}, wantOwner: "user:" + strings.Repeat("a", MaxOwnerLength-len("user:")), wantReason: "r"},
		{name: "missing reason", req: Request{Actor: alice}, wantErr: []string{"reason is required"}},
		{name: "blank reason", req: Request{Actor: alice, Reason: " \t\n"}, wantErr: []string{"reason is required"}},
		{name: "long reason", req: Request{Actor: alice, Reason: strings.Repeat("x", MaxReasonLength+1)}, wantErr: []string{"at most 500"}},
		{name: "control character in reason", req: Request{Actor: alice, Reason: "line\nbreak"}, wantErr: []string{"control characters"}},
		{name: "escape in reason", req: Request{Actor: alice, Reason: "bell\a"}, wantErr: []string{"control characters"}},
		{name: "invalid UTF-8 reason", req: Request{Actor: alice, Reason: "bad\xff"}, wantErr: []string{"UTF-8"}},
		{name: "missing actor", req: Request{Reason: "r"}, wantErr: []string{"unknown actor kind"}},
		{name: "actor without id", req: Request{Actor: core.Actor{Kind: core.ActorUser}, Reason: "r"}, wantErr: []string{"actor id must not be empty"}},
		{name: "long owner", req: Request{Actor: core.Actor{Kind: core.ActorUser, ID: strings.Repeat("a", MaxOwnerLength)}, Reason: "r"}, wantErr: []string{"owner must be at most 200"}},
		{name: "control character in actor", req: Request{Actor: core.Actor{Kind: core.ActorUser, ID: "al\x00ice"}, Reason: "r"}, wantErr: []string{"control characters"}},
		{name: "both expiries", req: Request{Actor: alice, Reason: "r", ExpiresIn: durationPtr(time.Hour), ExpiresAt: timePtr(fixedTime)}, wantErr: []string{"at most one"}},
		{name: "zero duration", req: Request{Actor: alice, Reason: "r", ExpiresIn: durationPtr(0)}, wantErr: []string{"must be positive"}},
		{name: "negative duration", req: Request{Actor: alice, Reason: "r", ExpiresIn: durationPtr(-time.Hour)}, wantErr: []string{"must be positive"}},
		{name: "invalid target", req: Request{Target: &core.Target{App: "web"}, Actor: alice, Reason: "r"}, wantErr: []string{"environment"}},
		{name: "every error", req: Request{Actor: alice, ExpiresIn: durationPtr(0)}, wantErr: []string{"reason is required", "must be positive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepo{}
			n := &recordingNotifier{}
			s := newService(t, repo, n, nil)
			lock, err := s.Lock(t.Context(), tt.req)
			if len(tt.wantErr) > 0 {
				if !errors.Is(err, core.ErrInvalidLock) {
					t.Fatalf("Lock = %v, want ErrInvalidLock", err)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if len(repo.acquired) != 0 || len(n.events) != 0 {
					t.Errorf("invalid request reached the repository (%d) or notifier (%d)", len(repo.acquired), len(n.events))
				}
				return
			}
			if err != nil {
				t.Fatalf("Lock: %v", err)
			}
			if len(repo.acquired) != 1 {
				t.Fatalf("repository called %d times", len(repo.acquired))
			}
			got := repo.acquired[0]
			if got.Owner != tt.wantOwner || got.Reason != tt.wantReason || got.ExpiresIn != tt.req.ExpiresIn || got.ExpiresAt != tt.req.ExpiresAt || got.Target != tt.req.Target {
				t.Errorf("repository request = %+v", got)
			}
			if lock.Owner != tt.wantOwner {
				t.Errorf("lock = %+v", lock)
			}
			want := []string{"locked " + lock.Scope() + " actor=" + tt.wantOwner}
			if !slices.Equal(n.summary(), want) {
				t.Errorf("notifications = %q, want %q", n.summary(), want)
			}
			if !n.events[0].OccurredAt.Equal(fixedTime) {
				t.Errorf("OccurredAt = %v", n.events[0].OccurredAt)
			}
		})
	}
}

func TestLockErrorsPassThrough(t *testing.T) {
	t.Parallel()

	held := &core.LockHeldError{Lock: core.Lock{Target: web, Owner: "bob", Reason: "first"}}
	tests := []struct {
		name   string
		err    error
		wantIs error
	}{
		{name: "held", err: held, wantIs: core.ErrLockHeld},
		{name: "invalid expiry", err: core.ErrInvalidLock, wantIs: core.ErrInvalidLock},
		{name: "database", err: errBoom, wantIs: errBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := &recordingNotifier{}
			s := newService(t, &fakeRepo{acquireErr: tt.err}, n, nil)
			_, err := s.Lock(t.Context(), Request{Target: &web, Actor: alice, Reason: "second"})
			if !errors.Is(err, tt.wantIs) {
				t.Errorf("Lock = %v, want %v", err, tt.wantIs)
			}
			if len(n.events) != 0 {
				t.Errorf("failed lock notified %v", n.summary())
			}
		})
	}
	s := newService(t, &fakeRepo{acquireErr: held}, nil, nil)
	_, err := s.Lock(t.Context(), Request{Target: &web, Actor: alice, Reason: "second"})
	if got, ok := errors.AsType[*core.LockHeldError](err); !ok || got.Lock.Owner != "bob" {
		t.Errorf("Lock = %v, want the held lock", err)
	}
}

func TestLockNotifiesPurgedExpiredFreezes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target *core.Target
		purged []core.Lock
		want   []string
	}{
		{name: "nothing purged", target: &web, want: []string{"locked web actor=user:alice"}},
		{
			name:   "app freeze purged",
			target: &web,
			purged: []core.Lock{{Target: web, Owner: "bob", Reason: "old"}},
			want:   []string{"expired web actor=", "locked web actor=user:alice"},
		},
		{
			name:   "global freeze purged",
			purged: []core.Lock{{Global: true, Owner: "bob", Reason: "old"}},
			want:   []string{"expired global actor=", "locked global actor=user:alice"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			n := &recordingNotifier{}
			s := newService(t, &fakeRepo{purged: tt.purged}, n, &logs)
			if _, err := s.Lock(t.Context(), Request{Target: tt.target, Actor: alice, Reason: "new", Role: core.RoleDeployer}); err != nil {
				t.Fatalf("Lock: %v", err)
			}
			if !slices.Equal(n.summary(), tt.want) {
				t.Errorf("notifications = %q, want %q", n.summary(), tt.want)
			}
			if got := strings.Count(logs.String(), "msg=\"lock expired\""); got != len(tt.purged) {
				t.Errorf("logged %d expiries, want %d:\n%s", got, len(tt.purged), logs.String())
			}
		})
	}
}

type contextNotifier struct {
	err         error
	hasDeadline bool
	deadline    time.Time
}

func (n *contextNotifier) NotifyLock(ctx context.Context, _ core.LockEvent) error {
	n.err = ctx.Err()
	n.deadline, n.hasDeadline = ctx.Deadline()
	return nil
}

func TestNotifyOutlivesTheRequest(t *testing.T) {
	t.Parallel()

	n := &contextNotifier{}
	s := newService(t, &fakeRepo{}, n, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := time.Now()
	if _, err := s.Unlock(ctx, &web, bob, core.RoleDeployer); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if n.err != nil {
		t.Errorf("notifier context error = %v, want none after the request was cancelled", n.err)
	}
	if !n.hasDeadline || n.deadline.Before(before) || n.deadline.After(time.Now().Add(NotifyTimeout)) {
		t.Errorf("notifier deadline = %v (set %v), want within %s", n.deadline, n.hasDeadline, NotifyTimeout)
	}
}

func TestUnlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		target       *core.Target
		actor        core.Actor
		repoErr      error
		wantIs       error
		wantReleased []string
		wantNotified []string
	}{
		{name: "app", target: &web, actor: bob, wantReleased: []string{"web by user:bob"}, wantNotified: []string{"unlocked web actor=user:bob"}},
		{name: "global", actor: bob, wantReleased: []string{"global by user:bob"}, wantNotified: []string{"unlocked global actor=user:bob"}},
		{name: "not found", target: &web, actor: bob, repoErr: core.ErrNotFound, wantIs: core.ErrNotFound, wantReleased: []string{"web by user:bob"}},
		{name: "database error", actor: bob, repoErr: errBoom, wantIs: errBoom, wantReleased: []string{"global by user:bob"}},
		{name: "missing actor", target: &web, actor: core.Actor{}, wantIs: core.ErrInvalidLock},
		{name: "invalid target", target: &core.Target{App: "web"}, actor: bob, wantIs: core.ErrInvalidLock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepo{releaseErr: tt.repoErr}
			n := &recordingNotifier{}
			s := newService(t, repo, n, nil)
			lock, err := s.Unlock(t.Context(), tt.target, tt.actor, core.RoleDeployer)
			if tt.wantIs != nil {
				if !errors.Is(err, tt.wantIs) {
					t.Errorf("Unlock = %v, want %v", err, tt.wantIs)
				}
			} else if err != nil || lock.Owner != "alice" {
				t.Errorf("Unlock = %+v, %v", lock, err)
			}
			if !slices.Equal(repo.released, tt.wantReleased) {
				t.Errorf("released = %q, want %q", repo.released, tt.wantReleased)
			}
			if !slices.Equal(n.summary(), tt.wantNotified) {
				t.Errorf("notifications = %q, want %q", n.summary(), tt.wantNotified)
			}
		})
	}
}

func TestNotifierFailureIsLogged(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	n := &recordingNotifier{err: errBoom}
	s := newService(t, &fakeRepo{}, n, &logs)
	if _, err := s.Lock(t.Context(), Request{Target: &web, Actor: alice, Reason: "r"}); err != nil {
		t.Errorf("Lock with failing notifier: %v", err)
	}
	if _, err := s.Unlock(t.Context(), &web, bob, core.RoleDeployer); err != nil {
		t.Errorf("Unlock with failing notifier: %v", err)
	}
	if got := strings.Count(logs.String(), "lock notification failed"); got != 2 {
		t.Errorf("logged %d notification failures, want 2: %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "error=boom") {
		t.Errorf("logs do not carry the error: %s", logs.String())
	}
}

func TestListAndBlocking(t *testing.T) {
	t.Parallel()

	listed := []core.Lock{{Global: true, Owner: "a"}, {Target: web, Owner: "b"}}
	repo := &fakeRepo{listed: listed, blocking: listed[:1]}
	s := newService(t, repo, nil, nil)
	got, err := s.List(t.Context())
	if err != nil || len(got) != 2 {
		t.Errorf("List = %+v, %v", got, err)
	}
	blocking, err := s.Blocking(t.Context(), web)
	if err != nil || len(blocking) != 1 || repo.blockArg != web {
		t.Errorf("Blocking = %+v, %v (arg %v)", blocking, err, repo.blockArg)
	}

	failing := newService(t, &fakeRepo{listErr: errBoom}, nil, nil)
	if _, err := failing.List(t.Context()); !errors.Is(err, errBoom) {
		t.Errorf("List = %v, want boom", err)
	}
	if _, err := failing.Blocking(t.Context(), web); !errors.Is(err, errBoom) {
		t.Errorf("Blocking = %v, want boom", err)
	}
}

func TestReminderRunOnce(t *testing.T) {
	t.Parallel()

	expired := []core.Lock{{Target: web, Owner: "alice"}}
	due := []core.Lock{{Global: true, Owner: "bob"}, {Target: core.Target{App: "api", Environment: core.DefaultEnvironment}, Owner: "carol"}}
	tests := []struct {
		name         string
		every        time.Duration
		repo         *fakeRepo
		notifierErr  error
		wantErr      []error
		wantNotified []string
		wantRemind   []time.Duration
	}{
		{
			name:         "expired and reminded",
			every:        24 * time.Hour,
			repo:         &fakeRepo{expired: expired, due: due},
			wantNotified: []string{"expired web actor=", "reminded global actor=", "reminded api actor="},
			wantRemind:   []time.Duration{24 * time.Hour},
		},
		{
			name:         "reminders disabled still sweeps expiry",
			every:        0,
			repo:         &fakeRepo{expired: expired, due: due},
			wantNotified: []string{"expired web actor="},
		},
		{
			name:       "nothing due",
			every:      time.Hour,
			repo:       &fakeRepo{},
			wantRemind: []time.Duration{time.Hour},
		},
		{
			name:         "expiry failure does not stop reminders",
			every:        time.Hour,
			repo:         &fakeRepo{expireErr: errBoom, due: due[:1]},
			wantErr:      []error{errBoom},
			wantNotified: []string{"reminded global actor="},
			wantRemind:   []time.Duration{time.Hour},
		},
		{
			name:         "reminder failure reported",
			every:        time.Hour,
			repo:         &fakeRepo{expired: expired, remindErr: context.DeadlineExceeded},
			wantErr:      []error{context.DeadlineExceeded},
			wantNotified: []string{"expired web actor="},
			wantRemind:   []time.Duration{time.Hour},
		},
		{
			name:         "notifier failure does not fail the sweep",
			every:        time.Hour,
			repo:         &fakeRepo{expired: expired, due: due[:1]},
			notifierErr:  errBoom,
			wantNotified: []string{"expired web actor=", "reminded global actor="},
			wantRemind:   []time.Duration{time.Hour},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := &recordingNotifier{err: tt.notifierErr}
			r, err := NewReminder(ReminderOptions{Repository: tt.repo, Notifier: n, Every: tt.every, Now: func() time.Time { return fixedTime }})
			if err != nil {
				t.Fatalf("NewReminder: %v", err)
			}
			err = r.RunOnce(t.Context())
			if len(tt.wantErr) == 0 && err != nil {
				t.Errorf("RunOnce = %v", err)
			}
			for _, want := range tt.wantErr {
				if !errors.Is(err, want) {
					t.Errorf("RunOnce = %v, want %v", err, want)
				}
			}
			if !slices.Equal(n.summary(), tt.wantNotified) {
				t.Errorf("notifications = %q, want %q", n.summary(), tt.wantNotified)
			}
			if !slices.Equal(tt.repo.remindFor, tt.wantRemind) {
				t.Errorf("reminder intervals = %v, want %v", tt.repo.remindFor, tt.wantRemind)
			}
			if tt.repo.sweeps != 1 {
				t.Errorf("swept %d times, want 1", tt.repo.sweeps)
			}
			for _, ev := range n.events {
				if !ev.OccurredAt.Equal(fixedTime) {
					t.Errorf("OccurredAt = %v", ev.OccurredAt)
				}
			}
		})
	}
}

func TestReminderRun(t *testing.T) {
	t.Parallel()

	var logs syncBuffer
	repo := &fakeRepo{expireErr: errBoom}
	r, err := NewReminder(ReminderOptions{Repository: repo, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Every: time.Hour})
	if err != nil {
		t.Fatalf("NewReminder: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, time.Millisecond) }()

	deadline := time.After(5 * time.Second)
	for repo.sweepCount() < 3 {
		select {
		case <-deadline:
			t.Fatalf("swept %d times before the deadline", repo.sweepCount())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if !strings.Contains(logs.String(), "lock sweep failed") {
		t.Errorf("sweep error not logged: %s", logs.String())
	}
	if err := r.Run(t.Context(), 0); err == nil {
		t.Error("Run with a zero tick succeeded")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLockAuditEntry(t *testing.T) {
	t.Parallel()

	expires := fixedTime.Add(2 * time.Hour)
	tests := []struct {
		name       string
		req        Request
		wantTarget *core.Target
		wantDetail string
	}{
		{
			name:       "app",
			req:        Request{Target: &web, Reason: " migration ", Actor: alice, Role: core.RoleDeployer},
			wantTarget: &web,
			wantDetail: `{"reason":"migration","scope":"web"}`,
		},
		{
			name:       "global with expiry",
			req:        Request{Reason: "holiday", Actor: alice, Role: core.RoleAdmin, ExpiresAt: &expires},
			wantDetail: `{"reason":"holiday","scope":"global"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepo{}
			s := newService(t, repo, nil, nil)
			if _, err := s.Lock(t.Context(), tt.req); err != nil {
				t.Fatalf("Lock: %v", err)
			}
			if len(repo.audits) != 1 || repo.commits != 1 {
				t.Fatalf("audits = %d, commits = %d, want one each", len(repo.audits), repo.commits)
			}
			got := repo.audits[0]
			if got.Action != core.AuditLockAcquire || got.Actor != tt.req.Actor || got.Role != tt.req.Role {
				t.Errorf("audit = %+v", got)
			}
			if !reflect.DeepEqual(got.Target, tt.wantTarget) {
				t.Errorf("target = %v, want %v", got.Target, tt.wantTarget)
			}
			if got.Validate() != nil {
				t.Errorf("audit entry is invalid: %v", got.Validate())
			}
			if tt.req.ExpiresAt == nil && string(got.Detail) != tt.wantDetail {
				t.Errorf("detail = %s, want %s", got.Detail, tt.wantDetail)
			}
		})
	}
}

func TestLockAuditDetailCarriesExpiry(t *testing.T) {
	t.Parallel()

	repo := &fakeRepo{}
	s := newService(t, repo, nil, nil)
	hour := time.Hour
	if _, err := s.Lock(t.Context(), Request{Reason: "r", Actor: alice, Role: core.RoleDeployer, ExpiresIn: &hour}); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(repo.audits[0].Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if _, ok := detail["expires_at"]; ok {
		t.Errorf("detail has expires_at although the repository returned no expiry: %v", detail)
	}
}

func TestUnlockAuditEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     *core.Target
		wantTarget *core.Target
		wantDetail string
	}{
		{name: "app", target: &web, wantTarget: &web, wantDetail: `{"locked_at":"0001-01-01T00:00:00Z","owner":"alice","reason":"held","scope":"web"}`},
		{name: "global", wantDetail: `{"locked_at":"0001-01-01T00:00:00Z","owner":"alice","reason":"held","scope":"global"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo := &fakeRepo{}
			s := newService(t, repo, nil, nil)
			if _, err := s.Unlock(t.Context(), tt.target, bob, core.RoleDeployer); err != nil {
				t.Fatalf("Unlock: %v", err)
			}
			if len(repo.audits) != 1 {
				t.Fatalf("audits = %d, want 1", len(repo.audits))
			}
			got := repo.audits[0]
			if got.Action != core.AuditLockRelease || got.Actor != bob || got.Role != core.RoleDeployer {
				t.Errorf("audit = %+v", got)
			}
			if !reflect.DeepEqual(got.Target, tt.wantTarget) {
				t.Errorf("target = %v, want %v", got.Target, tt.wantTarget)
			}
			if string(got.Detail) != tt.wantDetail {
				t.Errorf("detail = %s, want %s", got.Detail, tt.wantDetail)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("audit entry is invalid: %v", err)
			}
		})
	}
}

func TestFailedLockChangesWriteNoAudit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		repo *fakeRepo
		run  func(*Service) error
	}{
		{name: "lock held", repo: &fakeRepo{acquireErr: &core.LockHeldError{}}, run: func(s *Service) error {
			_, err := s.Lock(t.Context(), Request{Target: &web, Reason: "r", Actor: alice, Role: core.RoleDeployer})
			return err
		}},
		{name: "lock invalid", repo: &fakeRepo{}, run: func(s *Service) error {
			_, err := s.Lock(t.Context(), Request{Target: &web, Actor: alice, Role: core.RoleDeployer})
			return err
		}},
		{name: "lock audit fails", repo: &fakeRepo{auditErr: errBoom}, run: func(s *Service) error {
			_, err := s.Lock(t.Context(), Request{Target: &web, Reason: "r", Actor: alice, Role: core.RoleDeployer})
			return err
		}},
		{name: "unlock not found", repo: &fakeRepo{releaseErr: core.ErrNotFound}, run: func(s *Service) error {
			_, err := s.Unlock(t.Context(), &web, bob, core.RoleDeployer)
			return err
		}},
		{name: "unlock invalid actor", repo: &fakeRepo{}, run: func(s *Service) error {
			_, err := s.Unlock(t.Context(), &web, core.Actor{}, core.RoleDeployer)
			return err
		}},
		{name: "unlock audit fails", repo: &fakeRepo{auditErr: errBoom}, run: func(s *Service) error {
			_, err := s.Unlock(t.Context(), &web, bob, core.RoleDeployer)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := &recordingNotifier{}
			s := newService(t, tt.repo, n, nil)
			if err := tt.run(s); err == nil {
				t.Fatal("call succeeded")
			}
			if len(tt.repo.audits) != 0 || tt.repo.commits != 0 {
				t.Errorf("audits = %d, commits = %d, want none", len(tt.repo.audits), tt.repo.commits)
			}
			if len(n.events) != 0 {
				t.Errorf("failed call notified %v", n.summary())
			}
		})
	}
}
