package locks_test

import (
	"errors"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/locks"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestPostgresLockAudit(t *testing.T) {
	t.Parallel()

	alice := core.Actor{Kind: core.ActorUser, ID: "alice"}
	ci := core.Actor{Kind: core.ActorToken, ID: "ci"}
	blog := core.AppTarget("blog")
	tests := []struct {
		name   string
		target *core.Target
	}{
		{name: "app", target: &blog},
		{name: "global"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			svc, err := locks.New(locks.Options{Repository: locks.NewRepository(st)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			if _, err := svc.Lock(t.Context(), locks.Request{Target: tt.target, Reason: "migration", Actor: alice, Role: core.RoleDeployer}); err != nil {
				t.Fatalf("Lock: %v", err)
			}
			if _, err := svc.Lock(t.Context(), locks.Request{Target: tt.target, Reason: "second", Actor: ci, Role: core.RoleAdmin}); !errors.Is(err, core.ErrLockHeld) {
				t.Fatalf("second Lock = %v, want ErrLockHeld", err)
			}
			if entries := listAudit(t, st); len(entries) != 1 {
				t.Fatalf("audit rows after a refused lock = %d, want 1", len(entries))
			}

			if _, err := svc.Unlock(t.Context(), tt.target, ci, core.RoleAdmin); err != nil {
				t.Fatalf("Unlock: %v", err)
			}
			if _, err := svc.Unlock(t.Context(), tt.target, ci, core.RoleAdmin); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("second Unlock = %v, want ErrNotFound", err)
			}
			entries := listAudit(t, st)
			if len(entries) != 2 {
				t.Fatalf("audit rows = %d, want 2", len(entries))
			}
			want := []struct {
				action core.AuditAction
				actor  core.Actor
				role   core.Role
			}{
				{core.AuditLockRelease, ci, core.RoleAdmin},
				{core.AuditLockAcquire, alice, core.RoleDeployer},
			}
			for i, w := range want {
				got := entries[i]
				if got.Action != w.action || got.Actor.Kind != w.actor.Kind || got.Actor.ID != w.actor.ID || got.Role != w.role {
					t.Errorf("entry %d = %+v, want %+v", i, got, w)
				}
				if (got.Target == nil) != (tt.target == nil) || (tt.target != nil && *got.Target != *tt.target) {
					t.Errorf("entry %d target = %v, want %v", i, got.Target, tt.target)
				}
			}
		})
	}
}

func listAudit(t *testing.T, st *store.Store) []core.AuditEntry {
	t.Helper()
	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	return entries
}
