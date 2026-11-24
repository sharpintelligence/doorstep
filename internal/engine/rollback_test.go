package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
)

const withheldSuffix = "; automatic rollback withheld: release contains database migrations; roll back with --confirm-migration-rollback"

type autoRollbackEvent struct {
	Outcome                engine.RollbackOutcome `json:"outcome"`
	RollbackID             *core.DeploymentID     `json:"rollback_id"`
	ToDeploymentID         *core.DeploymentID     `json:"to_deployment_id"`
	Digest                 core.Digest            `json:"digest"`
	MigrationDeploymentIDs []core.DeploymentID    `json:"migration_deployment_ids"`
}

func autoRollbackData(t *testing.T, tr engine.Transition) *autoRollbackEvent {
	t.Helper()
	var data struct {
		RollbackEligible bool               `json:"rollback_eligible"`
		AutoRollback     *autoRollbackEvent `json:"auto_rollback"`
	}
	if err := json.Unmarshal(tr.Data, &data); err != nil {
		t.Fatalf("decode event data %s: %v", tr.Data, err)
	}
	return data.AutoRollback
}

func withRollbacks(h *harness, enabled bool) *fakeRollbacks {
	r := &fakeRollbacks{store: h.store, enabled: enabled}
	h.options.Rollbacks = r
	return r
}

func good(h *harness) core.Deployment {
	d := in(core.StateSucceeded, deploy())
	d.ID = 41
	d.Digest = digestOld
	d.Commit.SHA = repinSHA
	return h.store.add(d)
}

func rollbackOf(bad core.Deployment, to core.Deployment) *core.Deployment {
	return &core.Deployment{
		ID:           43,
		Target:       bad.Target,
		Kind:         core.KindRollback,
		Source:       core.SourceSystem,
		RequestedBy:  "system:doorstep",
		Commit:       to.Commit,
		Image:        to.Image,
		Digest:       to.Digest,
		RollbackOfID: &bad.ID,
	}
}

func failingAfterGates(h *harness) core.Deployment {
	h.gates.set(core.GatePhaseAfter, failedGate("e2e", "checkout broken"))
	return h.store.add(in(core.StateVerifying, deploy()))
}

func TestHandleRollsBackAutomaticallyWhenAnAfterGateFails(t *testing.T) {
	t.Parallel()

	h := newHarness()
	rollbacks := withRollbacks(h, true)
	to := good(h)
	d := failingAfterGates(h)
	rollbacks.plan = engine.RollbackPlan{Outcome: engine.RollbackQueued, To: &to, Rollback: rollbackOf(d, to)}

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	requests := rollbacks.received()
	if len(requests) != 1 {
		t.Fatalf("FailAndRollBack calls = %d, want 1", len(requests))
	}
	req := requests[0]
	if req.Deployment.ID != d.ID || req.Trigger != engine.RollbackTriggerGateFailed {
		t.Errorf("request = deployment %d trigger %s, want %d gate_failed", req.Deployment.ID, req.Trigger, d.ID)
	}
	if want := fmt.Sprintf("automatic rollback of deployment %d: after gates failed:", d.ID); !strings.HasPrefix(req.Reason, want) || !strings.Contains(req.Reason, "checkout broken") {
		t.Errorf("block reason = %q, want it to start with %q and name the gate", req.Reason, want)
	}

	got := h.store.deployment(d.ID)
	if got.State != core.StateFailed || !strings.HasPrefix(got.FailureReason, "after gates failed:") {
		t.Fatalf("deployment = %s %q, want failed with the gate failure as reason", got.State, got.FailureReason)
	}
	recorded := h.store.recorded()
	last := recorded[len(recorded)-1]
	sentence := "rolling back automatically to sha256:111111111111 (deployment 41) as deployment 43"
	if want := "failed while verifying; " + sentence; last.Message != want {
		t.Errorf("event message = %q, want %q", last.Message, want)
	}
	data := autoRollbackData(t, last)
	if data == nil || data.Outcome != engine.RollbackQueued || data.RollbackID == nil || *data.RollbackID != 43 ||
		data.ToDeploymentID == nil || *data.ToDeploymentID != 41 || data.Digest != digestOld || len(data.MigrationDeploymentIDs) != 0 {
		t.Errorf("auto_rollback = %+v", data)
	}
	if !lastEventData(t, h).RollbackEligible {
		t.Error("rollback_eligible = false")
	}

	notes := h.notifier.all()
	if kinds := h.notifier.kinds(); !slices.Equal(kinds, []core.NotificationKind{core.NotificationDeployFailed, core.NotificationDeployQueued}) {
		t.Fatalf("notifications = %v, want deploy.failed then deploy.queued", kinds)
	}
	if !strings.HasSuffix(notes[0].Message, sentence) {
		t.Errorf("deploy.failed message = %q, want it to end with %q", notes[0].Message, sentence)
	}
	if notes[1].Deployment.ID != 43 || notes[1].RollbackOf == nil || notes[1].RollbackOf.ID != d.ID || notes[1].Actor != "system:doorstep" {
		t.Errorf("deploy.queued = %+v rolling back %v", notes[1].Deployment, notes[1].RollbackOf)
	}
	if outcomes := h.metrics.outcomes(); !slices.Equal(outcomes, []engine.Outcome{engine.OutcomeFailed}) {
		t.Errorf("finished metrics = %v", outcomes)
	}
}

func TestHandleAutomaticRollbackAnnouncesCancelledDuplicates(t *testing.T) {
	t.Parallel()

	h := newHarness()
	rollbacks := withRollbacks(h, true)
	to := good(h)
	duplicate := h.store.add(in(core.StateCancelled, deploy()))
	d := failingAfterGates(h)
	rollbacks.plan = engine.RollbackPlan{Outcome: engine.RollbackQueued, To: &to, Rollback: rollbackOf(d, to)}
	rollbacks.cancelled = []core.DeploymentID{duplicate.ID}

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := []core.NotificationKind{core.NotificationDeployFailed, core.NotificationDeployQueued, core.NotificationDeployCancelled}
	if kinds := h.notifier.kinds(); !slices.Equal(kinds, want) {
		t.Fatalf("notifications = %v, want %v", kinds, want)
	}
	if note := h.notifier.all()[2]; note.Deployment.ID != duplicate.ID {
		t.Errorf("deploy.cancelled for %d, want %d", note.Deployment.ID, duplicate.ID)
	}
	if outcomes := h.metrics.outcomes(); !slices.Equal(outcomes, []engine.Outcome{engine.OutcomeFailed, engine.OutcomeCancelled}) {
		t.Errorf("finished metrics = %v", outcomes)
	}
}

func TestHandleAutomaticRollbackOfSyncFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		decision engine.Decision
	}{
		{name: "retries exhausted", decision: engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionRetriesExhausted, Class: core.FailureTransient}},
		{name: "version caused", decision: engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionVersionCaused, Class: core.FailureVersion}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			rollbacks := withRollbacks(h, true)
			to := good(h)
			h.sync.waitFn = degradedSync
			h.policy.decide = func(engine.Failure) engine.Decision { return tt.decision }
			d := h.store.add(in(core.StateSyncing, deploy()))
			rollbacks.plan = engine.RollbackPlan{Outcome: engine.RollbackQueued, To: &to, Rollback: rollbackOf(d, to)}

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			requests := rollbacks.received()
			if len(requests) != 1 || requests[0].Trigger != engine.RollbackTriggerSyncFailed {
				t.Fatalf("requests = %+v, want one with trigger sync_failed", requests)
			}
			if got := h.store.deployment(d.ID); got.State != core.StateFailed {
				t.Errorf("state = %s, want failed", got.State)
			}
			data := lastEventData(t, h)
			if data.Decision != tt.decision.Reason || !data.RollbackEligible {
				t.Errorf("event data = %+v", data)
			}
			if kinds := h.notifier.kinds(); !slices.Equal(kinds, []core.NotificationKind{core.NotificationDeployFailed, core.NotificationDeployQueued}) {
				t.Errorf("notifications = %v", kinds)
			}
		})
	}
}

func TestHandleDoesNotRollBackAutomatically(t *testing.T) {
	t.Parallel()

	rollback := func() core.Deployment {
		rb := deploy()
		rb.Kind = core.KindRollback
		rb.Digest = digestOld
		return rb
	}
	tests := []struct {
		name    string
		enabled bool
		nilPort bool
		setup   func(h *harness) core.Deployment
	}{
		{
			name:  "app has not opted in",
			setup: failingAfterGates,
		},
		{
			name:    "no port",
			enabled: true,
			nilPort: true,
			setup:   failingAfterGates,
		},
		{
			name:    "before gate failed",
			enabled: true,
			setup: func(h *harness) core.Deployment {
				h.gates.set(core.GatePhaseBefore, failedGate("checks", "ci red"))
				return h.store.add(deploy())
			},
		},
		{
			name:    "pin refused",
			enabled: true,
			setup: func(h *harness) core.Deployment {
				h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
					return engine.PinResult{}, fmt.Errorf("branch protected: %w", engine.ErrPermanent)
				}
				return h.store.add(in(core.StatePinning, deploy()))
			},
		},
		{
			name:    "configuration sync failure",
			enabled: true,
			setup: func(h *harness) core.Deployment {
				h.sync.waitFn = func(context.Context, string) (core.SyncStatus, error) {
					return core.SyncStatus{Failure: &core.SyncFailure{Reason: core.SyncReasonForbidden, Class: core.FailureConfiguration, Message: "rbac"}},
						fmt.Errorf("argocd: %w", core.ErrSyncForbidden)
				}
				return h.store.add(in(core.StateSyncing, deploy()))
			},
		},
		{
			name:    "newer release queued",
			enabled: true,
			setup: func(h *harness) core.Deployment {
				h.sync.waitFn = degradedSync
				h.policy.decide = func(engine.Failure) engine.Decision {
					return engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionNewerReleaseQueued, Class: core.FailureTransient}
				}
				return h.store.add(in(core.StateSyncing, deploy()))
			},
		},
		{
			name:    "rollback failed its after gates",
			enabled: true,
			setup: func(h *harness) core.Deployment {
				h.gates.set(core.GatePhaseAfter, failedGate("health", "degraded"))
				return h.store.add(in(core.StateVerifying, rollback()))
			},
		},
		{
			name:    "rollback failed to sync",
			enabled: true,
			setup: func(h *harness) core.Deployment {
				h.sync.waitFn = degradedSync
				h.policy.decide = func(engine.Failure) engine.Decision {
					return engine.Decision{Action: engine.FailureActionFail, Reason: engine.DecisionVersionCaused, Class: core.FailureVersion}
				}
				return h.store.add(in(core.StateSyncing, rollback()))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			rollbacks := withRollbacks(h, tt.enabled)
			if tt.nilPort {
				h.options.Rollbacks = nil
			}
			d := tt.setup(h)

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := h.store.deployment(d.ID); got.State != core.StateFailed {
				t.Fatalf("state = %s, want failed", got.State)
			}
			if requests := rollbacks.received(); len(requests) != 0 {
				t.Errorf("FailAndRollBack called %d times, want never", len(requests))
			}
			recorded := h.store.recorded()
			last := recorded[len(recorded)-1]
			if data := autoRollbackData(t, last); data != nil {
				t.Errorf("auto_rollback = %+v, want absent", data)
			}
			if strings.Contains(last.Message, "automatic") {
				t.Errorf("event message = %q mentions an automatic rollback", last.Message)
			}
			if kinds := h.notifier.kinds(); slices.Contains(kinds, core.NotificationDeployQueued) {
				t.Errorf("notifications = %v, want no deploy.queued", kinds)
			}
		})
	}
}

func TestHandleAutomaticRollbackOutcomes(t *testing.T) {
	t.Parallel()

	to := core.Deployment{ID: 41, Digest: digestOld}
	tests := []struct {
		name        string
		plan        engine.RollbackPlan
		gateMessage string
		wantMessage string
		wantData    autoRollbackEvent
		withheld    bool
	}{
		{
			name:        "withheld for migrations",
			plan:        engine.RollbackPlan{Outcome: engine.RollbackWithheldMigrations, To: &to, Migrations: []core.DeploymentID{44}},
			gateMessage: "checkout broken",
			wantMessage: "automatic rollback withheld: release contains database migrations (deployment 44); roll back with --confirm-migration-rollback",
			wantData:    autoRollbackEvent{Outcome: engine.RollbackWithheldMigrations, ToDeploymentID: &to.ID, Digest: digestOld, MigrationDeploymentIDs: []core.DeploymentID{44}},
			withheld:    true,
		},
		{
			name:        "withheld keeps the hint after a long reason",
			plan:        engine.RollbackPlan{Outcome: engine.RollbackWithheldMigrations, To: &to, Migrations: []core.DeploymentID{44, 46}},
			gateMessage: strings.Repeat("é", 600),
			wantMessage: "automatic rollback withheld: release contains database migrations (deployments 44, 46); roll back with --confirm-migration-rollback",
			wantData:    autoRollbackEvent{Outcome: engine.RollbackWithheldMigrations, ToDeploymentID: &to.ID, Digest: digestOld, MigrationDeploymentIDs: []core.DeploymentID{44, 46}},
			withheld:    true,
		},
		{
			name:        "no target",
			plan:        engine.RollbackPlan{Outcome: engine.RollbackNoTarget},
			gateMessage: "checkout broken",
			wantMessage: "automatic rollback not possible: no earlier successful deployment",
			wantData:    autoRollbackEvent{Outcome: engine.RollbackNoTarget},
		},
		{
			name:        "same version",
			plan:        engine.RollbackPlan{Outcome: engine.RollbackSameVersion, To: &to},
			gateMessage: "checkout broken",
			wantMessage: "automatic rollback skipped: this digest is the last one that succeeded",
			wantData:    autoRollbackEvent{Outcome: engine.RollbackSameVersion, ToDeploymentID: &to.ID, Digest: digestOld},
		},
		{
			name:        "rollback in progress",
			plan:        engine.RollbackPlan{Outcome: engine.RollbackInProgress},
			gateMessage: "checkout broken",
			wantMessage: "automatic rollback skipped: a rollback is already in progress",
			wantData:    autoRollbackEvent{Outcome: engine.RollbackInProgress},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			rollbacks := withRollbacks(h, true)
			rollbacks.plan = tt.plan
			h.gates.set(core.GatePhaseAfter, failedGate("e2e", tt.gateMessage))
			d := h.store.add(in(core.StateVerifying, deploy()))

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := h.store.deployment(d.ID)
			if got.State != core.StateFailed {
				t.Fatalf("state = %s, want failed", got.State)
			}
			switch {
			case tt.withheld && (!strings.HasSuffix(got.FailureReason, withheldSuffix) || !strings.HasPrefix(got.FailureReason, "after gates failed:")):
				t.Errorf("failure reason = %q, want the gate failure followed by the withheld hint", got.FailureReason)
			case tt.withheld && utf8.RuneCountInString(got.FailureReason) > 500:
				t.Errorf("failure reason has %d characters, want at most 500", utf8.RuneCountInString(got.FailureReason))
			case !tt.withheld && strings.Contains(got.FailureReason, "automatic"):
				t.Errorf("failure reason = %q, want the original reason only", got.FailureReason)
			}
			recorded := h.store.recorded()
			last := recorded[len(recorded)-1]
			if want := "failed while verifying; " + tt.wantMessage; last.Message != want {
				t.Errorf("event message = %q, want %q", last.Message, want)
			}
			data := autoRollbackData(t, last)
			if data == nil {
				t.Fatal("auto_rollback is missing")
			}
			if data.Outcome != tt.wantData.Outcome || data.Digest != tt.wantData.Digest || data.RollbackID != nil ||
				!equalID(data.ToDeploymentID, tt.wantData.ToDeploymentID) || !slices.Equal(data.MigrationDeploymentIDs, tt.wantData.MigrationDeploymentIDs) {
				t.Errorf("auto_rollback = %+v, want %+v", data, tt.wantData)
			}
			notes := h.notifier.all()
			if kinds := h.notifier.kinds(); !slices.Equal(kinds, []core.NotificationKind{core.NotificationDeployFailed}) {
				t.Fatalf("notifications = %v, want only deploy.failed", kinds)
			}
			if !strings.HasSuffix(notes[0].Message, tt.wantMessage) {
				t.Errorf("deploy.failed message = %q, want it to end with %q", notes[0].Message, tt.wantMessage)
			}
		})
	}
}

func equalID(a, b *core.DeploymentID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestHandleAutomaticRollbackErrors(t *testing.T) {
	t.Parallel()

	errDown := errors.New("database down")
	tests := []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "state conflict stops quietly", err: fmt.Errorf("transition: %w", core.ErrStateConflict)},
		{name: "other errors are returned", err: errDown, wantErr: errDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			rollbacks := withRollbacks(h, true)
			rollbacks.err = tt.err
			d := failingAfterGates(h)

			_, err := h.run(t, t.Context(), d.ID)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("Handle = %v, want %v", err, tt.wantErr)
			}
			if got := h.store.deployment(d.ID); got.State != core.StateVerifying {
				t.Errorf("state = %s, want still verifying", got.State)
			}
			if kinds := h.notifier.kinds(); len(kinds) != 0 {
				t.Errorf("notifications = %v, want none", kinds)
			}
			if outcomes := h.metrics.outcomes(); len(outcomes) != 0 {
				t.Errorf("finished metrics = %v, want none", outcomes)
			}
		})
	}
}
