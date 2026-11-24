package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type fakeAutomatic struct {
	plan     rollback.Plan
	err      error
	requests []rollback.AutomaticRequest
	applied  []store.Transition
}

func (f *fakeAutomatic) Automatic(_ context.Context, req rollback.AutomaticRequest) (rollback.AutomaticResult, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return rollback.AutomaticResult{}, f.err
	}
	t, err := req.Fail(f.plan)
	if err != nil {
		return rollback.AutomaticResult{}, err
	}
	f.applied = append(f.applied, t)
	failed := req.Deployment
	failed.State = t.To
	failed.FailureReason = t.FailureReason
	return rollback.AutomaticResult{Plan: f.plan, Failed: failed, Cancelled: []core.DeploymentID{7}}, nil
}

func autoRollbackCatalog(t *testing.T) catalogApps {
	t.Helper()
	return newCatalogApps(loadCatalog(t, `
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    auto_rollback: true
  - name: shop
    source:
      repo: example-user/shop
    image:
      repository: ghcr.io/example-user/shop
    gitops:
      repo: example-user/homelab
      path: apps/shop
`))
}

func TestAutoRollbacksEnabled(t *testing.T) {
	t.Parallel()

	apps := autoRollbackCatalog(t)
	tests := []struct {
		name      string
		rollbacks automaticRollbacker
		target    core.Target
		want      bool
	}{
		{name: "opted in", rollbacks: &fakeAutomatic{}, target: core.AppTarget("blog"), want: true},
		{name: "not opted in", rollbacks: &fakeAutomatic{}, target: core.AppTarget("shop")},
		{name: "unknown app", rollbacks: &fakeAutomatic{}, target: core.AppTarget("wiki")},
		{name: "no rollback service", target: core.AppTarget("blog")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (autoRollbacks{apps: apps, rollbacks: tt.rollbacks}).Enabled(tt.target); got != tt.want {
				t.Errorf("Enabled(%s) = %t, want %t", tt.target, got, tt.want)
			}
		})
	}
}

func TestAutoRollbacksFailAndRollBack(t *testing.T) {
	t.Parallel()

	bad := core.Deployment{ID: 42, Target: core.AppTarget("blog"), Kind: core.KindDeploy, State: core.StateVerifying}
	to := core.Deployment{ID: 41, Digest: core.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")}
	rb := core.Deployment{ID: 43, Kind: core.KindRollback}
	retries := 2
	tests := []struct {
		name    string
		trigger engine.RollbackTrigger
		plan    rollback.Plan
		want    engine.RollbackPlan
	}{
		{
			name:    "queued after a gate failure",
			trigger: engine.RollbackTriggerGateFailed,
			plan:    rollback.Plan{Outcome: rollback.OutcomeQueued, To: &to, Rollback: &rb},
			want:    engine.RollbackPlan{Outcome: engine.RollbackQueued, To: &to, Rollback: &rb},
		},
		{
			name:    "withheld after a sync failure",
			trigger: engine.RollbackTriggerSyncFailed,
			plan:    rollback.Plan{Outcome: rollback.OutcomeWithheldMigrations, To: &to, Migrations: []core.Deployment{{ID: 40}, {ID: 42}}},
			want:    engine.RollbackPlan{Outcome: engine.RollbackWithheldMigrations, To: &to, Migrations: []core.DeploymentID{40, 42}},
		},
		{
			name:    "no target",
			trigger: engine.RollbackTriggerGateFailed,
			plan:    rollback.Plan{Outcome: rollback.OutcomeNoTarget},
			want:    engine.RollbackPlan{Outcome: engine.RollbackNoTarget},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAutomatic{plan: tt.plan}
			var seen []engine.RollbackPlan
			transition := engine.Transition{
				ID: bad.ID, From: bad.State, To: core.StateFailed, Actor: engine.Actor, Message: "failed while verifying",
				Data: json.RawMessage(`{"stage":"verifying"}`), FailureReason: "after gates failed", ExpectRetries: &retries,
			}
			got, err := autoRollbacks{apps: autoRollbackCatalog(t), rollbacks: fake}.FailAndRollBack(t.Context(), engine.AutoRollbackRequest{
				Deployment: bad,
				Trigger:    tt.trigger,
				Reason:     "automatic rollback of deployment 42: after gates failed",
				Fail: func(p engine.RollbackPlan) (engine.Transition, error) {
					seen = append(seen, p)
					return transition, nil
				},
			})
			if err != nil {
				t.Fatalf("FailAndRollBack: %v", err)
			}
			req := fake.requests[0]
			if req.Deployment.ID != bad.ID || string(req.Trigger) != string(tt.trigger) || req.Reason != "automatic rollback of deployment 42: after gates failed" ||
				req.Actor != (core.Actor{Kind: core.ActorSystem, ID: "doorstep"}) {
				t.Errorf("request = %+v", req)
			}
			if len(seen) != 1 || !reflect.DeepEqual(seen[0], tt.want) {
				t.Errorf("Fail saw %+v, want %+v", seen, tt.want)
			}
			if want := storeTransition(transition); len(fake.applied) != 1 || !reflect.DeepEqual(fake.applied[0], want) {
				t.Errorf("applied %+v, want %+v", fake.applied, want)
			}
			if !reflect.DeepEqual(got.Plan, tt.want) || got.Failed.State != core.StateFailed || !slices.Equal(got.Cancelled, []core.DeploymentID{7}) {
				t.Errorf("result = %+v", got)
			}
		})
	}
}

func TestAutoRollbacksPassErrorsThrough(t *testing.T) {
	t.Parallel()

	errEncode := errors.New("encode")
	tests := []struct {
		name    string
		err     error
		failErr error
		want    error
	}{
		{name: "service error", err: core.ErrStateConflict, want: core.ErrStateConflict},
		{name: "failure transition error", failErr: errEncode, want: errEncode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAutomatic{err: tt.err, plan: rollback.Plan{Outcome: rollback.OutcomeNoTarget}}
			_, err := autoRollbacks{rollbacks: fake}.FailAndRollBack(t.Context(), engine.AutoRollbackRequest{
				Deployment: core.Deployment{ID: 1},
				Trigger:    engine.RollbackTriggerGateFailed,
				Fail: func(engine.RollbackPlan) (engine.Transition, error) {
					return engine.Transition{}, tt.failErr
				},
			})
			if !errors.Is(err, tt.want) {
				t.Errorf("FailAndRollBack = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEngineRollbackValuesAreRollbackValues(t *testing.T) {
	t.Parallel()

	outcomes := map[engine.RollbackOutcome]rollback.Outcome{
		engine.RollbackQueued:             rollback.OutcomeQueued,
		engine.RollbackWithheldMigrations: rollback.OutcomeWithheldMigrations,
		engine.RollbackNoTarget:           rollback.OutcomeNoTarget,
		engine.RollbackSameVersion:        rollback.OutcomeSameVersion,
		engine.RollbackInProgress:         rollback.OutcomeRollbackInProgress,
	}
	for in, want := range outcomes {
		if rollback.Outcome(in) != want {
			t.Errorf("engine outcome %q does not match rollback outcome %q", in, want)
		}
	}
	triggers := map[engine.RollbackTrigger]rollback.Trigger{
		engine.RollbackTriggerGateFailed: rollback.TriggerGateFailed,
		engine.RollbackTriggerSyncFailed: rollback.TriggerSyncFailed,
	}
	for in, want := range triggers {
		if rollback.Trigger(in) != want {
			t.Errorf("engine trigger %q does not match rollback trigger %q", in, want)
		}
	}
}

func TestRollbackReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		trigger rollback.Trigger
		want    metrics.RollbackReason
	}{
		{trigger: rollback.TriggerManual, want: metrics.RollbackReasonManual},
		{trigger: rollback.TriggerGateFailed, want: metrics.RollbackReasonGateFailed},
		{trigger: rollback.TriggerSyncFailed, want: metrics.RollbackReasonSyncFailed},
	}
	for _, tt := range tests {
		if got := rollbackReason(tt.trigger); got != tt.want {
			t.Errorf("rollbackReason(%s) = %s, want %s", tt.trigger, got, tt.want)
		}
	}
}
