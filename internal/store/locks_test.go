package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func appTarget(app string) *core.Target {
	return &core.Target{App: app, Environment: "prod"}
}

func freeze(target *core.Target, owner, reason string) core.LockRequest {
	return core.LockRequest{Target: target, Owner: owner, Reason: reason}
}

func durationPtr(d time.Duration) *time.Duration { return &d }

func timePtr(t time.Time) *time.Time { return &t }

func mustFreeze(t *testing.T, st *store.Store, req core.LockRequest) core.Lock {
	t.Helper()
	lock, err := st.AcquireFreeze(t.Context(), req)
	if err != nil {
		t.Fatalf("AcquireFreeze(%s): %v", req.Scope(), err)
	}
	return lock
}

const expireFreezes = "UPDATE locks SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE kind = 'freeze'"

func lockEvents(t *testing.T, st *store.Store) []core.LockEvent {
	t.Helper()
	events, err := st.ListLockEvents(t.Context(), 100)
	if err != nil {
		t.Fatalf("ListLockEvents: %v", err)
	}
	return events
}

func eventSummary(events []core.LockEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = fmt.Sprintf("%s %s owner=%s actor=%s", ev.Action, ev.Lock.Scope(), ev.Lock.Owner, ev.Actor)
	}
	return out
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s =\n%q\nwant\n%q", name, got, want)
	}
}

func TestAcquireFreeze(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		req   core.LockRequest
		check func(t *testing.T, lock core.Lock)
	}{
		{
			name: "app without expiry",
			req:  freeze(appTarget("web"), "alice", "database migration"),
			check: func(t *testing.T, lock core.Lock) {
				t.Helper()
				if lock.Global || lock.Target != *appTarget("web") || lock.ExpiresAt != nil {
					t.Errorf("lock = %+v", lock)
				}
			},
		},
		{
			name: "global",
			req:  freeze(nil, "bob", "holiday"),
			check: func(t *testing.T, lock core.Lock) {
				t.Helper()
				if !lock.Global || lock.Target != (core.Target{}) || lock.Scope() != core.GlobalScope {
					t.Errorf("lock = %+v", lock)
				}
			},
		},
		{
			name: "expires in",
			req:  core.LockRequest{Target: appTarget("web"), Owner: "alice", Reason: "r", ExpiresIn: durationPtr(2 * time.Hour)},
			check: func(t *testing.T, lock core.Lock) {
				t.Helper()
				if lock.ExpiresAt == nil || lock.ExpiresAt.Sub(lock.CreatedAt) != 2*time.Hour {
					t.Errorf("expires_at = %v, created_at = %v, want two hours later", lock.ExpiresAt, lock.CreatedAt)
				}
			},
		},
		{
			name: "expires at",
			req:  core.LockRequest{Owner: "alice", Reason: "r", ExpiresAt: timePtr(time.Date(2100, 1, 2, 3, 4, 5, 0, time.UTC))},
			check: func(t *testing.T, lock core.Lock) {
				t.Helper()
				if lock.ExpiresAt == nil || !lock.ExpiresAt.Equal(time.Date(2100, 1, 2, 3, 4, 5, 0, time.UTC)) {
					t.Errorf("expires_at = %v", lock.ExpiresAt)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			lock := mustFreeze(t, st, tt.req)
			if lock.Kind != core.LockFreeze || lock.Owner != tt.req.Owner || lock.Reason != tt.req.Reason || lock.CreatedAt.IsZero() || lock.RemindedAt != nil || lock.DeploymentID != nil {
				t.Errorf("lock = %+v", lock)
			}
			tt.check(t, lock)

			listed, err := st.ListFreezes(t.Context())
			if err != nil || len(listed) != 1 || listed[0].Scope() != lock.Scope() {
				t.Errorf("ListFreezes = %+v, %v", listed, err)
			}
			assertStrings(t, "events", eventSummary(lockEvents(t, st)), []string{
				fmt.Sprintf("locked %s owner=%s actor=%s", lock.Scope(), tt.req.Owner, tt.req.Owner),
			})
		})
	}
}

func TestAcquireFreezeRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		req    core.LockRequest
		wantIs error
	}{
		{name: "blank owner", req: freeze(appTarget("web"), " ", "r"), wantIs: core.ErrInvalidLock},
		{name: "invalid target", req: freeze(&core.Target{App: "web"}, "alice", "r")},
		{name: "both expiries", req: core.LockRequest{Owner: "alice", ExpiresIn: durationPtr(time.Hour), ExpiresAt: timePtr(time.Now().Add(time.Hour))}, wantIs: core.ErrInvalidLock},
		{name: "zero duration", req: core.LockRequest{Owner: "alice", ExpiresIn: durationPtr(0)}, wantIs: core.ErrInvalidLock},
		{name: "negative duration", req: core.LockRequest{Owner: "alice", ExpiresIn: durationPtr(-time.Minute)}, wantIs: core.ErrInvalidLock},
		{name: "expiry in the past", req: core.LockRequest{Target: appTarget("web"), Owner: "alice", ExpiresAt: timePtr(time.Now().Add(-time.Minute))}, wantIs: core.ErrInvalidLock},
		{name: "global expiry in the past", req: core.LockRequest{Owner: "alice", ExpiresAt: timePtr(time.Now().Add(-time.Minute))}, wantIs: core.ErrInvalidLock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			_, err := st.AcquireFreeze(t.Context(), tt.req)
			if err == nil {
				t.Fatal("AcquireFreeze succeeded")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tt.wantIs)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM locks"); n != 0 {
				t.Errorf("%d locks written", n)
			}
			if n := queryInt(t, st, "SELECT count(*) FROM lock_events"); n != 0 {
				t.Errorf("%d lock events written", n)
			}
		})
	}
}

func TestAcquireFreezeConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target *core.Target
	}{
		{name: "app", target: appTarget("web")},
		{name: "global", target: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			held := mustFreeze(t, st, freeze(tt.target, "alice", "first"))
			_, err := st.AcquireFreeze(t.Context(), freeze(tt.target, "bob", "second"))
			heldErr, ok := errors.AsType[*core.LockHeldError](err)
			if !ok || !errors.Is(err, core.ErrLockHeld) {
				t.Fatalf("AcquireFreeze = %v, want LockHeldError", err)
			}
			if heldErr.Lock.Owner != "alice" || heldErr.Lock.Reason != "first" || !heldErr.Lock.CreatedAt.Equal(held.CreatedAt) {
				t.Errorf("held lock = %+v, want %+v", heldErr.Lock, held)
			}
			assertStrings(t, "events", eventSummary(lockEvents(t, st)), []string{
				fmt.Sprintf("locked %s owner=alice actor=alice", held.Scope()),
			})
		})
	}
}

func TestAppAndGlobalFreezesCoexist(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustFreeze(t, st, freeze(appTarget("web"), "alice", "web"))
	mustFreeze(t, st, freeze(appTarget("api"), "alice", "api"))
	mustFreeze(t, st, freeze(nil, "bob", "everything"))
	mustFreeze(t, st, freeze(&core.Target{App: "web", Environment: "staging"}, "carol", "staging"))

	locks, err := st.ListFreezes(t.Context())
	if err != nil {
		t.Fatalf("ListFreezes: %v", err)
	}
	var got []string
	for _, l := range locks {
		got = append(got, l.Scope()+"/"+l.Target.Environment)
	}
	assertStrings(t, "ListFreezes", got, []string{"global/", "api/prod", "web/prod", "web/staging"})

	blocking, err := st.BlockingFreezes(t.Context(), *appTarget("web"))
	if err != nil {
		t.Fatalf("BlockingFreezes: %v", err)
	}
	got = nil
	for _, l := range blocking {
		got = append(got, l.Scope()+":"+l.Reason)
	}
	assertStrings(t, "BlockingFreezes", got, []string{"global:everything", "web:web"})

	blocking, err = st.BlockingFreezes(t.Context(), *appTarget("unknown"))
	if err != nil || len(blocking) != 1 || !blocking[0].Global {
		t.Errorf("BlockingFreezes(unknown) = %+v, %v; want the global freeze", blocking, err)
	}
	if _, err := st.BlockingFreezes(t.Context(), core.Target{}); err == nil {
		t.Error("BlockingFreezes accepted an empty target")
	}
}

func TestExpiredFreeze(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target *core.Target
	}{
		{name: "app", target: appTarget("web")},
		{name: "global", target: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			mustFreeze(t, st, core.LockRequest{Target: tt.target, Owner: "alice", Reason: "old", ExpiresIn: durationPtr(time.Hour)})
			mustExec(t, st, expireFreezes)

			listed, err := st.ListFreezes(t.Context())
			if err != nil || len(listed) != 0 {
				t.Errorf("ListFreezes = %+v, %v; want none", listed, err)
			}
			blocking, err := st.BlockingFreezes(t.Context(), *appTarget("web"))
			if err != nil || len(blocking) != 0 {
				t.Errorf("BlockingFreezes = %+v, %v; want none", blocking, err)
			}
			if _, err := st.ReleaseFreeze(t.Context(), tt.target, "carol"); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("ReleaseFreeze of expired lock = %v, want ErrNotFound", err)
			}

			mustFreeze(t, st, freeze(tt.target, "alice", "old"))
			mustExec(t, st, expireFreezes)
			replacement := mustFreeze(t, st, freeze(tt.target, "bob", "new"))
			if replacement.Owner != "bob" {
				t.Errorf("replacement = %+v", replacement)
			}
			scope := replacement.Scope()
			assertStrings(t, "events", eventSummary(lockEvents(t, st)), []string{
				"locked " + scope + " owner=bob actor=bob",
				"expired " + scope + " owner=alice actor=",
				"locked " + scope + " owner=alice actor=alice",
				"expired " + scope + " owner=alice actor=",
				"locked " + scope + " owner=alice actor=alice",
			})
		})
	}
}

func TestReleaseFreeze(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target *core.Target
	}{
		{name: "app", target: appTarget("web")},
		{name: "global", target: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			mustFreeze(t, st, freeze(appTarget("other"), "zed", "unrelated"))
			held := mustFreeze(t, st, core.LockRequest{Target: tt.target, Owner: "alice", Reason: "work", ExpiresIn: durationPtr(time.Hour)})

			released, err := st.ReleaseFreeze(t.Context(), tt.target, "bob")
			if err != nil {
				t.Fatalf("ReleaseFreeze: %v", err)
			}
			if released.Owner != "alice" || released.Reason != "work" || released.Scope() != held.Scope() || !released.ExpiresAt.Equal(*held.ExpiresAt) {
				t.Errorf("released = %+v, want %+v", released, held)
			}
			if _, err := st.ReleaseFreeze(t.Context(), tt.target, "bob"); !errors.Is(err, core.ErrNotFound) {
				t.Errorf("second ReleaseFreeze = %v, want ErrNotFound", err)
			}
			listed, err := st.ListFreezes(t.Context())
			if err != nil || len(listed) != 1 || listed[0].Scope() != "other" {
				t.Errorf("ListFreezes = %+v, %v; want only the unrelated freeze", listed, err)
			}
			events := lockEvents(t, st)
			assertStrings(t, "events", eventSummary(events), []string{
				"unlocked " + held.Scope() + " owner=alice actor=bob",
				"locked " + held.Scope() + " owner=alice actor=alice",
				"locked other owner=zed actor=zed",
			})
			if events[0].Lock.Reason != "work" || events[0].Lock.ExpiresAt == nil || events[0].OccurredAt.IsZero() {
				t.Errorf("unlock event = %+v", events[0])
			}
			mustFreeze(t, st, freeze(tt.target, "carol", "again"))
		})
	}
}

func TestReleaseFreezeNotFound(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustFreeze(t, st, freeze(appTarget("web"), "alice", "web"))
	tests := []struct {
		name   string
		target *core.Target
	}{
		{name: "unknown app", target: appTarget("nope")},
		{name: "known app without lock", target: &core.Target{App: "web", Environment: "staging"}},
		{name: "global", target: nil},
	}
	for _, tt := range tests {
		if _, err := st.ReleaseFreeze(t.Context(), tt.target, "bob"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s: ReleaseFreeze = %v, want ErrNotFound", tt.name, err)
		}
	}
	if _, err := st.ReleaseFreeze(t.Context(), &core.Target{App: "web"}, "bob"); err == nil || errors.Is(err, core.ErrNotFound) {
		t.Errorf("ReleaseFreeze with invalid target = %v, want validation error", err)
	}
	if n := countApps(t, st, "nope"); n != 0 {
		t.Errorf("ReleaseFreeze created %d apps rows", n)
	}
}

func TestDeleteExpiredFreezes(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustFreeze(t, st, core.LockRequest{Target: appTarget("web"), Owner: "alice", Reason: "a", ExpiresIn: durationPtr(time.Hour)})
	mustFreeze(t, st, core.LockRequest{Owner: "bob", Reason: "b", ExpiresIn: durationPtr(time.Hour)})
	mustFreeze(t, st, core.LockRequest{Target: appTarget("api"), Owner: "carol", Reason: "c", ExpiresIn: durationPtr(time.Hour)})
	mustFreeze(t, st, freeze(appTarget("db"), "dave", "forever"))
	mustExec(t, st, "UPDATE locks SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE owner IN ('alice', 'bob')")

	expired, err := st.DeleteExpiredFreezes(t.Context())
	if err != nil {
		t.Fatalf("DeleteExpiredFreezes: %v", err)
	}
	var got []string
	for _, l := range expired {
		got = append(got, l.Scope())
	}
	assertStrings(t, "expired", got, []string{"global", "web"})

	again, err := st.DeleteExpiredFreezes(t.Context())
	if err != nil || len(again) != 0 {
		t.Errorf("second DeleteExpiredFreezes = %+v, %v", again, err)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM locks"); n != 2 {
		t.Errorf("%d locks remain, want 2", n)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM lock_events WHERE action = 'expired'"); n != 2 {
		t.Errorf("%d expired events, want 2", n)
	}
}

func TestClaimLockReminders(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	mustFreeze(t, st, freeze(appTarget("web"), "alice", "long"))
	mustFreeze(t, st, freeze(nil, "bob", "global"))
	mustFreeze(t, st, freeze(appTarget("api"), "carol", "fresh"))
	mustFreeze(t, st, core.LockRequest{Target: appTarget("db"), Owner: "dave", Reason: "expired", ExpiresIn: durationPtr(time.Hour)})
	mustExec(t, st, "UPDATE locks SET created_at = now() - interval '25 hours' WHERE owner IN ('alice', 'bob', 'dave')")
	mustExec(t, st, "UPDATE locks SET expires_at = now() - interval '1 second' WHERE owner = 'dave'")

	claim := func() []string {
		t.Helper()
		locks, err := st.ClaimLockReminders(t.Context(), 24*time.Hour)
		if err != nil {
			t.Fatalf("ClaimLockReminders: %v", err)
		}
		var got []string
		for _, l := range locks {
			if l.RemindedAt == nil {
				t.Errorf("%s: RemindedAt not set", l.Scope())
			}
			got = append(got, l.Scope())
		}
		return got
	}
	assertStrings(t, "first claim", claim(), []string{"global", "web"})
	assertStrings(t, "second claim", claim(), nil)

	mustExec(t, st, "UPDATE locks SET reminded_at = now() - interval '25 hours' WHERE owner = 'alice'")
	assertStrings(t, "after another interval", claim(), []string{"web"})

	if n := queryInt(t, st, "SELECT count(*) FROM lock_events WHERE action = 'reminded'"); n != 3 {
		t.Errorf("%d reminded events, want 3", n)
	}
	if _, err := st.ClaimLockReminders(t.Context(), 0); err == nil {
		t.Error("ClaimLockReminders(0) succeeded")
	}
}

func TestClaimLockRemindersConcurrent(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for i := range 5 {
		mustFreeze(t, st, freeze(appTarget(fmt.Sprintf("app-%d", i)), "alice", "long"))
	}
	mustExec(t, st, "UPDATE locks SET created_at = now() - interval '2 hours'")

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			locks, err := st.ClaimLockReminders(t.Context(), time.Hour)
			if err != nil {
				t.Errorf("ClaimLockReminders: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, l := range locks {
				seen[l.Scope()]++
			}
		})
	}
	close(start)
	wg.Wait()
	if len(seen) != 5 {
		t.Errorf("reminded %d locks, want 5: %v", len(seen), seen)
	}
	for scope, n := range seen {
		if n != 1 {
			t.Errorf("%s reminded %d times", scope, n)
		}
	}
}

func TestAcquireFreezeConcurrent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target *core.Target
	}{
		{name: "app", target: appTarget("web")},
		{name: "global", target: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			var wg sync.WaitGroup
			var mu sync.Mutex
			won, held := 0, 0
			start := make(chan struct{})
			for i := range 6 {
				wg.Go(func() {
					<-start
					_, err := st.AcquireFreeze(t.Context(), freeze(tt.target, fmt.Sprintf("owner-%d", i), "race"))
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						won++
					case errors.Is(err, core.ErrLockHeld):
						held++
					default:
						t.Errorf("AcquireFreeze: %v", err)
					}
				})
			}
			close(start)
			wg.Wait()
			if won != 1 || held != 5 {
				t.Errorf("won %d, held %d; want 1 and 5", won, held)
			}
		})
	}
}

func TestListLockEventsRejectsBadLimit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	if _, err := st.ListLockEvents(t.Context(), 0); err == nil {
		t.Error("ListLockEvents(0) succeeded")
	}
}
