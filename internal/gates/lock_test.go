package gates_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

func freeze(owner, reason string, global bool) core.Lock {
	return core.Lock{
		Target:    core.Target{App: "web", Environment: "prod"},
		Global:    global,
		Kind:      core.LockFreeze,
		Owner:     owner,
		Reason:    reason,
		CreatedAt: time.Date(2026, 11, 23, 9, 30, 0, 0, time.FixedZone("CET", 3600)),
	}
}

func TestLockGate(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	withExpiry := freeze("alice", "incident 42", false)
	withExpiry.ExpiresAt = &expires
	many := make([]core.Lock, 0, 60)
	for i := range 60 {
		many = append(many, freeze(fmt.Sprintf("user%d", i), "", false))
	}

	tests := []struct {
		name          string
		locks         []core.Lock
		wantStatus    core.GateStatus
		wantMessage   string
		wantLocks     int
		wantTruncated float64
		wantFirst     map[string]any
	}{
		{
			name:        "no freeze passes",
			wantStatus:  core.GatePassed,
			wantMessage: "no freeze",
		},
		{
			name:        "app freeze holds",
			locks:       []core.Lock{withExpiry},
			wantStatus:  core.GatePending,
			wantMessage: "frozen by alice: incident 42",
			wantLocks:   1,
			wantFirst: map[string]any{
				"scope": "web", "owner": "alice", "reason": "incident 42",
				"created_at": "2026-11-23T08:30:00Z", "expires_at": "2026-11-24T09:00:00Z",
			},
		},
		{
			name:        "global freeze without reason",
			locks:       []core.Lock{freeze("bob", "", true), withExpiry},
			wantStatus:  core.GatePending,
			wantMessage: "globally frozen by bob (and 1 more freeze)",
			wantLocks:   2,
			wantFirst:   map[string]any{"scope": "global", "owner": "bob", "created_at": "2026-11-23T08:30:00Z"},
		},
		{
			name:          "many freezes are truncated in evidence",
			locks:         many,
			wantStatus:    core.GatePending,
			wantMessage:   "frozen by user0 (and 59 more freezes)",
			wantLocks:     50,
			wantTruncated: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			f.freezes.locks = tt.locks
			g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateLock})
			d := deployment(core.KindDeploy)
			ev := evaluateGate(t, g, d, nil)
			if ev.Status != tt.wantStatus || ev.Message != tt.wantMessage {
				t.Errorf("Evaluate = %s %q, want %s %q", ev.Status, ev.Message, tt.wantStatus, tt.wantMessage)
			}
			if f.freezes.target != d.Target {
				t.Errorf("Blocking called for %v, want %v", f.freezes.target, d.Target)
			}
			evidence := evidenceMap(t, ev)
			locks, ok := evidence["locks"].([]any)
			if !ok || len(locks) != tt.wantLocks {
				t.Fatalf("evidence locks = %v, want %d", evidence["locks"], tt.wantLocks)
			}
			if got, _ := evidence["truncated"].(float64); got != tt.wantTruncated {
				t.Errorf("truncated = %v, want %v", got, tt.wantTruncated)
			}
			if tt.wantFirst == nil {
				return
			}
			first := locks[0].(map[string]any)
			if len(first) != len(tt.wantFirst) {
				t.Errorf("first lock evidence = %v, want %v", first, tt.wantFirst)
			}
			for k, want := range tt.wantFirst {
				if first[k] != want {
					t.Errorf("first lock %s = %v, want %v", k, first[k], want)
				}
			}
		})
	}
}

func TestLockGateReadError(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	boom := errors.New("database down")
	f.freezes.err = boom
	g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateLock})
	_, err := g.Evaluate(t.Context(), gates.Request{Deployment: deployment(core.KindDeploy), Phase: core.GatePhaseBefore})
	if !errors.Is(err, boom) {
		t.Errorf("Evaluate error = %v, want %v", err, boom)
	}
}
