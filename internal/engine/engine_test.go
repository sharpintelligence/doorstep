package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
)

func TestNewRequiresPorts(t *testing.T) {
	t.Parallel()

	_, err := engine.New(engine.Options{})
	if err == nil {
		t.Fatal("New without ports succeeded")
	}
	for _, want := range []string{"deployment store", "digest resolver", "pin writer", "sync watcher", "gate evaluator", "block list", "sync timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestHandleDeploySucceeds(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.blocks.trailers = core.BlockTrailers{Blocks: []core.Digest{digestBad}}
	d := h.store.add(deploy())

	result, err := h.run(t, t.Context(), d.ID)
	if err != nil || result != (engine.Result{}) {
		t.Fatalf("Handle = %+v, %v", result, err)
	}

	wantStates := []core.State{core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying, core.StateSucceeded}
	if got := h.store.states(); !slices.Equal(got, wantStates) {
		t.Errorf("transitions = %v, want %v", got, wantStates)
	}
	for _, tr := range h.store.recorded() {
		if tr.Actor != engine.Actor {
			t.Errorf("transition %s -> %s actor = %q, want %q", tr.From, tr.To, tr.Actor, engine.Actor)
		}
	}
	got := h.store.deployment(d.ID)
	if got.Digest != digestNew || got.PinCommitSHA != pinSHA || got.PreviousDigest != digestOld || got.GitOpsRepository != gitopsRepo || got.ArgoCDRevision != argoSHA {
		t.Errorf("lineage = digest %s pin %s previous %s repo %s revision %s", got.Digest, got.PinCommitSHA, got.PreviousDigest, got.GitOpsRepository, got.ArgoCDRevision)
	}
	if h.resolver.calls() != 1 || h.resolver.refs[0] != d.Image {
		t.Errorf("resolved %v, want %v once", h.resolver.refs, d.Image)
	}
	if h.blocks.checks() != 1 {
		t.Errorf("Blocked calls = %d, want 1", h.blocks.checks())
	}
	pins := h.pins.pinRequests()
	if len(pins) != 1 || pins[0].Digest != digestNew || pins[0].Deployment.ID != d.ID || !slices.Equal(pins[0].Blocks, []core.Digest{digestBad}) {
		t.Errorf("pin requests = %+v", pins)
	}
	if len(h.blocks.mirrored) != 1 || h.blocks.mirrored[0].commit.SHA != pinSHA {
		t.Errorf("mirrored = %+v, want one call with the pin commit", h.blocks.mirrored)
	}
	if waits := h.sync.waitCalls(); len(waits) != 1 || waits[0].revision != pinSHA {
		t.Errorf("WaitSynced calls = %+v", waits)
	}
	if got := h.gates.evaluated(); !slices.Equal(got, []core.GatePhase{core.GatePhaseBefore, core.GatePhaseAfter}) {
		t.Errorf("gate phases = %v", got)
	}
	if got := h.notifier.kinds(); !slices.Equal(got, []core.NotificationKind{core.NotificationDeployStarted, core.NotificationDeploySucceeded}) {
		t.Errorf("notifications = %v", got)
	}
	if got := h.metrics.startedApps(); !slices.Equal(got, []string{"blog"}) {
		t.Errorf("started metrics = %v", got)
	}
	if got := h.metrics.outcomes(); !slices.Equal(got, []engine.Outcome{engine.OutcomeSucceeded}) {
		t.Errorf("finished metrics = %v", got)
	}
	if len(h.pins.abandoned()) != 0 {
		t.Errorf("abandoned = %+v, want none", h.pins.abandoned())
	}
}

func TestHandleRollbackSucceeds(t *testing.T) {
	t.Parallel()

	h := newHarness()
	bad := h.store.add(in(core.StateCancelled, deploy()))
	rb := deploy()
	rb.Kind = core.KindRollback
	rb.Digest = digestOld
	rb.RollbackOfID = &bad.ID
	d := h.store.add(rb)

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSucceeded || got.Digest != digestOld {
		t.Fatalf("rollback = %s with digest %s", got.State, got.Digest)
	}
	if h.resolver.calls() != 0 || h.blocks.checks() != 0 {
		t.Errorf("rollback resolved %d times and checked blocks %d times, want neither", h.resolver.calls(), h.blocks.checks())
	}
	pins := h.pins.pinRequests()
	if len(pins) != 2 || pins[0].Digest != digestOld || pins[0].Repin || pins[1].Digest != digestOld || !pins[1].Repin {
		t.Errorf("pin requests = %+v, want the preset digest pinned and then confirmed", pins)
	}
	if len(h.gates.reevaluations()) != 0 {
		t.Errorf("after gates re-evaluated although the pin was not replaced")
	}
	notes := h.notifier.all()
	last := notes[len(notes)-1]
	if last.Kind != core.NotificationDeployRolledBack || last.RollbackOf == nil || last.RollbackOf.ID != bad.ID {
		t.Errorf("last notification = %s rolling back %v", last.Kind, last.RollbackOf)
	}
	if got := h.metrics.outcomes(); !slices.Equal(got, []engine.Outcome{engine.OutcomeRolledBack}) {
		t.Errorf("finished metrics = %v", got)
	}
}

func TestHandleRolledBackFallsBackWhenOriginalIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness()
	missing := core.DeploymentID(9999)
	rb := deploy()
	rb.Kind = core.KindRollback
	rb.Digest = digestOld
	rb.RollbackOfID = &missing
	d := h.store.add(rb)

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.notifier.kinds(); !slices.Equal(got, []core.NotificationKind{core.NotificationDeployStarted, core.NotificationDeploySucceeded}) {
		t.Errorf("notifications = %v, want succeeded without the rolled back deployment", got)
	}
}

type failureCase struct {
	name        string
	start       core.State
	kind        core.Kind
	setup       func(h *harness)
	wantStates  []core.State
	wantResult  engine.Result
	wantErr     bool
	wantStage   core.State
	wantClass   core.FailureClass
	wantReason  []string
	wantAbandon bool
	wantNotes   []core.NotificationKind
}

func TestHandleOutcomes(t *testing.T) {
	t.Parallel()

	transient := errors.New("connection reset")
	tests := []failureCase{
		{
			name:       "unknown app",
			setup:      func(h *harness) { h.apps = nil },
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateQueued,
			wantClass:  core.FailureConfiguration,
			wantReason: []string{"app blog is not configured"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:       "before gates pending",
			setup:      func(h *harness) { h.gates.set(core.GatePhaseBefore, pending()) },
			wantResult: engine.Result{RequeueAfter: engine.DefaultBeforePollInterval},
		},
		{
			name:       "before gates failed",
			setup:      func(h *harness) { h.gates.set(core.GatePhaseBefore, failedGate("ci", "test failed")) },
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateQueued,
			wantClass:  core.FailureUnknown,
			wantReason: []string{"before gates failed: ci: test failed"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:    "before gate store error",
			setup:   func(h *harness) { h.gates.err = transient },
			wantErr: true,
		},
		{
			name: "before gates report an unknown status",
			setup: func(h *harness) {
				h.gates.set(core.GatePhaseBefore, core.GateOutcome{Phase: core.GatePhaseBefore, Status: "maybe"})
			},
			wantErr: true,
		},
		{
			name:  "after gates report an unknown status",
			start: core.StateVerifying,
			setup: func(h *harness) {
				h.gates.set(core.GatePhaseAfter, core.GateOutcome{Phase: core.GatePhaseAfter, Status: "maybe"})
			},
			wantErr: true,
		},
		{
			name:  "pin reverted outside doorstep",
			start: core.StateVerifying,
			setup: func(h *harness) {
				h.pins.current = func(core.Deployment) (core.Digest, error) { return digestOld, nil }
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateVerifying,
			wantClass:  core.FailureUnknown,
			wantReason: []string{"no longer pins " + string(digestNew), "it pins " + string(digestOld), "reverted or replaced outside doorstep"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "pin replaced by a value that is not a digest",
			start: core.StateVerifying,
			setup: func(h *harness) {
				h.pins.current = func(core.Deployment) (core.Digest, error) { return "", nil }
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateVerifying,
			wantClass:  core.FailureUnknown,
			wantReason: []string{"it pins no digest"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "current pin unreadable",
			start: core.StateVerifying,
			setup: func(h *harness) {
				h.pins.current = func(core.Deployment) (core.Digest, error) { return "", transient }
			},
			wantErr: true,
		},
		{
			name:  "current pin gone",
			start: core.StateVerifying,
			setup: func(h *harness) {
				h.pins.current = func(core.Deployment) (core.Digest, error) {
					return "", fmt.Errorf("no kustomization.yaml: %w", engine.ErrPermanent)
				}
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateVerifying,
			wantClass:  core.FailureConfiguration,
			wantReason: []string{"read the current pin", "no kustomization.yaml"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "current pin unreadable past the deadline",
			start: core.StateVerifying,
			setup: func(h *harness) {
				h.pins.current = func(core.Deployment) (core.Digest, error) { return "", transient }
				h.clock.Advance(engine.DefaultVerifyTimeout)
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateVerifying,
			wantClass:  core.FailureTransient,
			wantReason: []string{"read the current pin: gave up after 1h0m0s", "connection reset"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "image not ready",
			start: core.StateResolving,
			setup: func(h *harness) {
				h.resolver.steps = []func() (core.Digest, error){func() (core.Digest, error) { return "", engine.ErrImageNotReady }}
			},
			wantResult: engine.Result{RequeueAfter: engine.DefaultResolveRetryInterval},
		},
		{
			name:  "image never became ready",
			start: core.StateResolving,
			setup: func(h *harness) {
				h.resolver.steps = []func() (core.Digest, error){func() (core.Digest, error) { return "", fmt.Errorf("manifest unknown: %w", engine.ErrImageNotReady) }}
				h.clock.Advance(engine.DefaultResolveTimeout)
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateResolving,
			wantClass:  core.FailureTransient,
			wantReason: []string{"was not available within 15m0s", "manifest unknown"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "invalid image reference",
			start: core.StateResolving,
			setup: func(h *harness) {
				h.resolver.steps = []func() (core.Digest, error){func() (core.Digest, error) { return "", fmt.Errorf("bad tag: %w", engine.ErrPermanent) }}
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateResolving,
			wantClass:  core.FailureConfiguration,
			wantReason: []string{"resolve ghcr.io/example-user/blog:" + appSHA, "bad tag"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "registry unavailable",
			start: core.StateResolving,
			setup: func(h *harness) {
				h.resolver.steps = []func() (core.Digest, error){func() (core.Digest, error) { return "", transient }}
			},
			wantErr: true,
		},
		{
			name:  "registry unavailable past the deadline",
			start: core.StateResolving,
			setup: func(h *harness) {
				h.resolver.steps = []func() (core.Digest, error){func() (core.Digest, error) { return "", transient }}
				h.clock.Advance(engine.DefaultResolveTimeout + time.Second)
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateResolving,
			wantClass:  core.FailureTransient,
			wantReason: []string{"gave up after 15m0s", "connection reset"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "blocked digest",
			start: core.StateResolving,
			setup: func(h *harness) {
				h.blocks.blocked = &core.Block{Target: target, Digest: digestNew, CreatedBy: "alice", Reason: "leaks memory"}
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateResolving,
			wantClass:  core.FailureVersion,
			wantReason: []string{string(digestNew), "blocked by alice: leaks memory"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:    "block lookup fails",
			start:   core.StateResolving,
			setup:   func(h *harness) { h.blocks.blockedErr = transient },
			wantErr: true,
		},
		{
			name:  "rollback without digest",
			start: core.StateResolving,
			kind:  core.KindRollback,
			setup: func(h *harness) {
				h.store.mu.Lock()
				defer h.store.mu.Unlock()
				for id, d := range h.store.deployments {
					if d.Kind == core.KindRollback {
						d.Digest = ""
						h.store.deployments[id] = d
					}
				}
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateResolving,
			wantClass:  core.FailureConfiguration,
			wantReason: []string{"rollback has no digest"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "pin refused",
			start: core.StatePinning,
			setup: func(h *harness) {
				h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
					return engine.PinResult{}, fmt.Errorf("branch protected: %w", engine.ErrPermanent)
				}
			},
			wantStates:  []core.State{core.StateFailed},
			wantStage:   core.StatePinning,
			wantClass:   core.FailureConfiguration,
			wantReason:  []string{"pin " + string(digestNew), "branch protected"},
			wantAbandon: true,
			wantNotes:   []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "pin needs a person",
			start: core.StatePinning,
			setup: func(h *harness) {
				h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
					return engine.PinResult{}, fmt.Errorf("pull request from a fork: %w", engine.ErrNeedsAttention)
				}
			},
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StatePinning,
			wantClass:  core.FailureConfiguration,
			wantReason: []string{"pin " + string(digestNew), "pull request from a fork"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:  "pin transient",
			start: core.StatePinning,
			setup: func(h *harness) {
				h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
					return engine.PinResult{}, transient
				}
			},
			wantErr: true,
		},
		{
			name:  "pin transient past the deadline",
			start: core.StatePinning,
			setup: func(h *harness) {
				h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
					return engine.PinResult{}, transient
				}
				h.clock.Advance(engine.DefaultPinTimeout)
			},
			wantStates:  []core.State{core.StateFailed},
			wantStage:   core.StatePinning,
			wantClass:   core.FailureTransient,
			wantReason:  []string{"gave up after 45m0s"},
			wantAbandon: true,
			wantNotes:   []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:    "pending trailers unavailable",
			start:   core.StatePinning,
			setup:   func(h *harness) { h.blocks.trailerErr = transient },
			wantErr: true,
		},
		{
			name:       "after gates pending",
			start:      core.StateVerifying,
			setup:      func(h *harness) { h.gates.set(core.GatePhaseAfter, pending()) },
			wantResult: engine.Result{RequeueAfter: engine.DefaultVerifyPollInterval},
		},
		{
			name:       "after gates failed",
			start:      core.StateVerifying,
			setup:      func(h *harness) { h.gates.set(core.GatePhaseAfter, failedGate("soak", "degraded")) },
			wantStates: []core.State{core.StateFailed},
			wantStage:  core.StateVerifying,
			wantClass:  core.FailureVersion,
			wantReason: []string{"after gates failed: soak: degraded"},
			wantNotes:  []core.NotificationKind{core.NotificationDeployFailed},
		},
		{
			name:    "after gate store error",
			start:   core.StateVerifying,
			setup:   func(h *harness) { h.gates.err = transient },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runFailureCase(t, tt)
		})
	}
}

func runFailureCase(t *testing.T, tt failureCase) {
	t.Helper()
	h := newHarness()
	start := tt.start
	if start == "" {
		start = core.StateQueued
	}
	d := deploy()
	if tt.kind == core.KindRollback {
		bad := h.store.add(in(core.StateCancelled, deploy()))
		d.Kind = core.KindRollback
		d.RollbackOfID = &bad.ID
		d.Digest = digestOld
	}
	d = h.store.add(in(start, d))
	if tt.setup != nil {
		tt.setup(h)
	}

	result, err := h.run(t, t.Context(), d.ID)
	if (err != nil) != tt.wantErr {
		t.Fatalf("Handle error = %v, want error %t", err, tt.wantErr)
	}
	if result != tt.wantResult {
		t.Errorf("Handle result = %+v, want %+v", result, tt.wantResult)
	}
	if got := h.store.states(); !slices.Equal(got, tt.wantStates) {
		t.Errorf("transitions = %v, want %v", got, tt.wantStates)
	}
	if got := h.notifier.kinds(); !slices.Equal(got, tt.wantNotes) {
		t.Errorf("notifications = %v, want %v", got, tt.wantNotes)
	}
	if got := len(h.pins.abandoned()) > 0; got != tt.wantAbandon {
		t.Errorf("abandoned = %t, want %t", got, tt.wantAbandon)
	}
	decided := h.policy.decided()
	if tt.wantStage == "" {
		if len(decided) != 0 {
			t.Errorf("policy consulted %+v, want no failure", decided)
		}
		return
	}
	if len(decided) != 1 {
		t.Fatalf("policy consulted %d times, want 1", len(decided))
	}
	f := decided[0]
	if f.Stage != tt.wantStage || f.Class != tt.wantClass || f.Deployment.ID != d.ID {
		t.Errorf("failure = stage %s class %s deployment %d, want %s %s %d", f.Stage, f.Class, f.Deployment.ID, tt.wantStage, tt.wantClass, d.ID)
	}
	got := h.store.deployment(d.ID)
	for _, want := range tt.wantReason {
		if !strings.Contains(got.FailureReason, want) {
			t.Errorf("failure reason %q does not contain %q", got.FailureReason, want)
		}
	}
	last := h.store.recorded()[len(h.store.recorded())-1]
	var data map[string]any
	if err := json.Unmarshal(last.Data, &data); err != nil || data["stage"] != string(tt.wantStage) || data["failure_class"] != string(tt.wantClass) {
		t.Errorf("failure event data = %s (%v)", last.Data, err)
	}
	wantOutcome := engine.OutcomeFailed
	if tt.kind == core.KindRollback {
		wantOutcome = engine.OutcomeRollbackFailed
	}
	if outcomes := h.metrics.outcomes(); !slices.Equal(outcomes, []engine.Outcome{wantOutcome}) {
		t.Errorf("finished metrics = %v, want %s", outcomes, wantOutcome)
	}
}

func TestHandleSyncFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		failure    *core.SyncFailure
		wantClass  core.FailureClass
		wantReason []string
		wantSync   core.SyncFailureReason
	}{
		{
			name:       "sync failed",
			err:        fmt.Errorf("argocd: %w: hook failed", core.ErrSyncFailed),
			failure:    &core.SyncFailure{Reason: core.SyncReasonFailed, Class: core.FailureVersion, Message: "hook failed"},
			wantClass:  core.FailureVersion,
			wantReason: []string{"Argo CD sync failed (sync_failed): hook failed"},
			wantSync:   core.SyncReasonFailed,
		},
		{
			name:       "timed out",
			err:        fmt.Errorf("argocd: %w after 10m", core.ErrSyncTimeout),
			failure:    &core.SyncFailure{Reason: core.SyncReasonDegraded, Class: core.FailureVersion, Message: "pod crashlooping"},
			wantClass:  core.FailureVersion,
			wantReason: []string{"within 10m0s", "(degraded): pod crashlooping"},
			wantSync:   core.SyncReasonDegraded,
		},
		{
			name:       "timed out without an observation",
			err:        core.ErrSyncTimeout,
			wantClass:  core.FailureUnknown,
			wantReason: []string{"(progressing): application did not settle"},
			wantSync:   core.SyncReasonProgressing,
		},
		{
			name:       "application not found",
			err:        fmt.Errorf("argocd: %w", core.ErrSyncTargetNotFound),
			wantClass:  core.FailureConfiguration,
			wantReason: []string{"Argo CD Application not found (not_found)"},
			wantSync:   core.SyncReasonNotFound,
		},
		{
			name:       "forbidden",
			err:        fmt.Errorf("argocd: %w", core.ErrSyncForbidden),
			failure:    &core.SyncFailure{Reason: core.SyncReasonForbidden, Class: core.FailureConfiguration, Message: "rbac"},
			wantClass:  core.FailureConfiguration,
			wantReason: []string{"forbidden (forbidden): rbac"},
			wantSync:   core.SyncReasonForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			h.sync.waitFn = func(context.Context, string) (core.SyncStatus, error) {
				return core.SyncStatus{Failure: tt.failure}, tt.err
			}
			d := h.store.add(in(core.StateSyncing, deploy()))

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := h.store.deployment(d.ID)
			if got.State != core.StateFailed {
				t.Fatalf("state = %s, want failed", got.State)
			}
			for _, want := range tt.wantReason {
				if !strings.Contains(got.FailureReason, want) {
					t.Errorf("reason %q does not contain %q", got.FailureReason, want)
				}
			}
			decided := h.policy.decided()
			if len(decided) != 1 || decided[0].Class != tt.wantClass || decided[0].Sync == nil || decided[0].Sync.Reason != tt.wantSync {
				t.Fatalf("policy saw %+v, want class %s sync reason %s", decided, tt.wantClass, tt.wantSync)
			}
			var data map[string]any
			recorded := h.store.recorded()
			if err := json.Unmarshal(recorded[len(recorded)-1].Data, &data); err != nil || data["sync_reason"] != string(tt.wantSync) || data["stage"] != "syncing" {
				t.Errorf("event data = %v (%v)", data, err)
			}
		})
	}
}

func TestHandleSyncTransientErrorRetries(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.sync.waitFn = func(context.Context, string) (core.SyncStatus, error) {
		return core.SyncStatus{}, errors.New("watch closed")
	}
	d := h.store.add(in(core.StateSyncing, deploy()))
	if _, err := h.run(t, t.Context(), d.ID); err == nil || !strings.Contains(err.Error(), "watch closed") {
		t.Fatalf("Handle error = %v, want the transient error", err)
	}
	if got := h.store.states(); len(got) != 0 {
		t.Errorf("transitions = %v, want none", got)
	}
}

func TestHandleSyncResumesWithRemainingTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{name: "fresh", elapsed: 0, want: syncTimeout},
		{name: "resumed", elapsed: 4 * time.Minute, want: 6 * time.Minute},
		{name: "expired gets the grace period", elapsed: time.Hour, want: engine.DefaultSyncGrace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			d := h.store.add(in(core.StateSyncing, deploy()))
			h.clock.Advance(tt.elapsed)
			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			waits := h.sync.waitCalls()
			if len(waits) != 1 {
				t.Fatalf("WaitSynced calls = %d", len(waits))
			}
			if diff := tt.want - waits[0].timeout; diff < 0 || diff > 5*time.Second {
				t.Errorf("WaitSynced got %s to wait, want about %s", waits[0].timeout, tt.want)
			}
		})
	}
}

func TestHandleSyncOwnDeadlineChecksOnceMore(t *testing.T) {
	t.Parallel()

	outOfSync := core.SyncStatus{Failure: &core.SyncFailure{Reason: core.SyncReasonOutOfSync, Class: core.FailureUnknown, Message: "still out of sync"}}
	tests := []struct {
		name       string
		check      func(string) (core.SyncStatus, error)
		wantState  core.State
		wantReason string
	}{
		{
			name:      "ready at the last read",
			check:     func(string) (core.SyncStatus, error) { return syncedStatus(), nil },
			wantState: core.StateSucceeded,
		},
		{
			name: "the last read explains the failure",
			check: func(string) (core.SyncStatus, error) {
				return core.SyncStatus{Failure: &core.SyncFailure{Reason: core.SyncReasonDegraded, Class: core.FailureVersion, Message: "crashloop"}}, nil
			},
			wantState:  core.StateFailed,
			wantReason: "within 10m0s (degraded): crashloop",
		},
		{
			name:       "the last read fails",
			check:      func(string) (core.SyncStatus, error) { return core.SyncStatus{}, errors.New("connection refused") },
			wantState:  core.StateFailed,
			wantReason: "within 10m0s (out_of_sync): still out of sync",
		},
		{
			name: "the Application is gone",
			check: func(string) (core.SyncStatus, error) {
				return core.SyncStatus{}, fmt.Errorf("argocd: %w", core.ErrSyncTargetNotFound)
			},
			wantState:  core.StateFailed,
			wantReason: "Argo CD Application not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			h.options.SyncGrace = 20 * time.Millisecond
			h.sync.waitFn = func(ctx context.Context, _ string) (core.SyncStatus, error) {
				<-ctx.Done()
				return outOfSync, ctx.Err()
			}
			h.sync.checkFn = tt.check
			d := h.store.add(in(core.StateSyncing, deploy()))
			h.clock.Advance(time.Hour)

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := h.store.deployment(d.ID)
			if got.State != tt.wantState || !strings.Contains(got.FailureReason, tt.wantReason) {
				t.Fatalf("deployment = %s %q, want %s %q", got.State, got.FailureReason, tt.wantState, tt.wantReason)
			}
			if !slices.Equal(h.sync.checked, []string{pinSHA}) {
				t.Errorf("Check calls = %v, want one for the pin commit", h.sync.checked)
			}
		})
	}
}

func TestHandleSyncUnobservedDeadlineReportsTheReadError(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.options.SyncGrace = 20 * time.Millisecond
	h.sync.waitFn = func(ctx context.Context, _ string) (core.SyncStatus, error) {
		<-ctx.Done()
		return core.SyncStatus{}, ctx.Err()
	}
	h.sync.checkFn = func(string) (core.SyncStatus, error) { return core.SyncStatus{}, errors.New("connection refused") }
	d := h.store.add(in(core.StateSyncing, deploy()))
	h.clock.Advance(time.Hour)

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := h.store.deployment(d.ID)
	if got.State != core.StateFailed || !strings.Contains(got.FailureReason, "(unavailable): connection refused") {
		t.Fatalf("deployment = %s %q", got.State, got.FailureReason)
	}
	if f := h.policy.decided(); len(f) != 1 || f[0].Class != core.SyncReasonUnavailable.Class(true) {
		t.Errorf("failures = %+v, want the unavailable class", f)
	}
}

func TestHandleShutdownIsNotAFailure(t *testing.T) {
	t.Parallel()

	for _, state := range []core.State{core.StateResolving, core.StatePinning, core.StateSyncing} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h.resolver.steps = []func() (core.Digest, error){func() (core.Digest, error) {
				cancel()
				return "", context.Canceled
			}}
			h.pins.pinFn = func(ctx context.Context, _ int, _ engine.PinRequest) (engine.PinResult, error) {
				cancel()
				return engine.PinResult{}, ctx.Err()
			}
			h.sync.waitFn = func(ctx context.Context, _ string) (core.SyncStatus, error) {
				cancel()
				return core.SyncStatus{}, ctx.Err()
			}
			d := h.store.add(in(state, deploy()))
			h.clock.Advance(24 * time.Hour)

			if _, err := h.run(t, ctx, d.ID); !errors.Is(err, context.Canceled) {
				t.Fatalf("Handle error = %v, want context.Canceled", err)
			}
			if got := h.store.states(); len(got) != 0 {
				t.Errorf("transitions = %v, want none", got)
			}
			if len(h.policy.decided()) != 0 || len(h.notifier.kinds()) != 0 || len(h.pins.abandoned()) != 0 {
				t.Errorf("shutdown failed, notified or abandoned: %+v %v %+v", h.policy.decided(), h.notifier.kinds(), h.pins.abandoned())
			}
		})
	}
}

func TestHandlePinIsBoundedByThePinDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		elapsed   time.Duration
		want      time.Duration
		wantState core.State
		wantErr   bool
	}{
		{name: "fresh", elapsed: 0, want: engine.DefaultPinTimeout, wantState: core.StatePinning, wantErr: true},
		{name: "resumed", elapsed: 40 * time.Minute, want: 5 * time.Minute, wantState: core.StatePinning, wantErr: true},
		{name: "expired gets the grace period", elapsed: engine.DefaultPinTimeout, want: engine.DefaultSyncGrace, wantState: core.StateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			var got time.Duration
			h.pins.pinFn = func(ctx context.Context, _ int, _ engine.PinRequest) (engine.PinResult, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					return engine.PinResult{}, errors.New("no deadline")
				}
				got = time.Until(deadline)
				return engine.PinResult{}, errors.New("merge pending")
			}
			d := h.store.add(in(core.StatePinning, deploy()))
			h.clock.Advance(tt.elapsed)

			_, err := h.run(t, t.Context(), d.ID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Handle error = %v, want error %t", err, tt.wantErr)
			}
			if diff := tt.want - got; diff < 0 || diff > 5*time.Second {
				t.Errorf("Pin got %s, want about %s", got, tt.want)
			}
			if state := h.store.deployment(d.ID).State; state != tt.wantState {
				t.Errorf("state = %s, want %s", state, tt.wantState)
			}
		})
	}
}

func TestHandleWithoutPinCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		health     []core.Health
		healthErr  error
		elapsed    time.Duration
		wantState  core.State
		wantResult engine.Result
		wantReason string
	}{
		{name: "healthy", wantState: core.StateSucceeded},
		{
			name:       "progressing",
			health:     []core.Health{{Status: core.HealthProgressing}},
			wantState:  core.StateSyncing,
			wantResult: engine.Result{RequeueAfter: engine.DefaultVerifyPollInterval},
		},
		{
			name:       "degraded past the timeout",
			health:     []core.Health{{Status: core.HealthDegraded, Message: "crashloop"}},
			elapsed:    syncTimeout,
			wantState:  core.StateFailed,
			wantReason: "(degraded): crashloop",
		},
		{
			name:       "unreadable past the timeout",
			healthErr:  errors.New("connection refused"),
			elapsed:    syncTimeout,
			wantState:  core.StateFailed,
			wantReason: "(unavailable): connection refused",
		},
		{
			name:       "application missing",
			healthErr:  fmt.Errorf("argocd: %w", core.ErrSyncTargetNotFound),
			wantState:  core.StateFailed,
			wantReason: "Argo CD Application not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			h.sync.health = tt.health
			h.sync.healthE = tt.healthErr
			d := in(core.StateSyncing, deploy())
			d.PinCommitSHA = ""
			d = h.store.add(d)
			h.clock.Advance(tt.elapsed)

			result, err := h.run(t, t.Context(), d.ID)
			if err != nil || result != tt.wantResult {
				t.Fatalf("Handle = %+v, %v, want %+v", result, err, tt.wantResult)
			}
			got := h.store.deployment(d.ID)
			if got.State != tt.wantState || !strings.Contains(got.FailureReason, tt.wantReason) {
				t.Fatalf("deployment = %s %q, want %s %q", got.State, got.FailureReason, tt.wantState, tt.wantReason)
			}
			if len(h.sync.waitCalls()) != 0 {
				t.Errorf("WaitSynced called without a pin commit")
			}
			if tt.wantState == core.StateSucceeded && h.store.recorded()[0].Message != "pin already current; no pin commit to wait for" {
				t.Errorf("verifying message = %q", h.store.recorded()[0].Message)
			}
		})
	}
}

func TestHandlePinWithoutCommitSkipsWait(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
		return engine.PinResult{Previous: digestNew}, nil
	}
	h.blocks.trailers = core.BlockTrailers{Unblocks: []core.Digest{digestBad}}
	d := h.store.add(in(core.StatePinning, deploy()))

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSucceeded || got.PinCommitSHA != "" {
		t.Fatalf("deployment = %s with pin %q", got.State, got.PinCommitSHA)
	}
	if len(h.blocks.mirrored) != 0 {
		t.Errorf("trailers marked mirrored without a changed pin: %+v", h.blocks.mirrored)
	}
}

func TestHandleMarkMirroredErrorIsNotFatal(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.blocks.trailers = core.BlockTrailers{Blocks: []core.Digest{digestBad}}
	h.blocks.markErr = errors.New("database down")
	d := h.store.add(in(core.StatePinning, deploy()))

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSucceeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
}

func TestHandleResumesFromEveryState(t *testing.T) {
	t.Parallel()

	all := []core.State{core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying, core.StateSucceeded}
	for i, start := range []core.State{core.StateQueued, core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying} {
		t.Run(string(start), func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			d := h.store.add(in(start, deploy()))
			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got, want := h.store.states(), all[i:]; !slices.Equal(got, want) {
				t.Errorf("transitions = %v, want %v", got, want)
			}
			wantStarted := 0
			if start == core.StateQueued {
				wantStarted = 1
			}
			if got := len(h.metrics.startedApps()); got != wantStarted {
				t.Errorf("started metrics = %d, want %d", got, wantStarted)
			}
			resolves, pins, waits := 0, 0, 0
			if i <= 1 {
				resolves = 1
			}
			if i <= 2 {
				pins = 1
			}
			if i <= 3 {
				waits = 1
			}
			if got := h.resolver.calls(); got != resolves {
				t.Errorf("resolves = %d, want %d: a resumed deployment must not resolve again", got, resolves)
			}
			if got := len(h.pins.pinRequests()); got != pins {
				t.Errorf("pins = %d, want %d: a resumed deployment must not pin again", got, pins)
			}
			if got := h.sync.waitCalls(); len(got) != waits || (waits == 1 && got[0].revision != pinSHA) {
				t.Errorf("sync waits = %+v, want %d for %s", got, waits, pinSHA)
			}
		})
	}

	for _, state := range core.TerminalStates() {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			d := h.store.add(in(state, deploy()))
			if result, err := h.run(t, t.Context(), d.ID); err != nil || result != (engine.Result{}) {
				t.Fatalf("Handle = %+v, %v", result, err)
			}
			if len(h.store.states()) != 0 || len(h.notifier.kinds()) != 0 {
				t.Errorf("terminal deployment changed: %v %v", h.store.states(), h.notifier.kinds())
			}
		})
	}
}

func TestHandleStopsOnStateConflict(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := h.store.add(in(core.StateResolving, deploy()))
	h.resolver.steps = []func() (core.Digest, error){func() (core.Digest, error) {
		h.store.setState(d.ID, core.StateCancelled)
		return digestNew, nil
	}}

	result, err := h.run(t, t.Context(), d.ID)
	if err != nil || result != (engine.Result{}) {
		t.Fatalf("Handle = %+v, %v, want a quiet stop", result, err)
	}
	if len(h.notifier.kinds()) != 0 || len(h.metrics.outcomes()) != 0 || len(h.policy.decided()) != 0 {
		t.Errorf("conflict announced something: %v %v %+v", h.notifier.kinds(), h.metrics.outcomes(), h.policy.decided())
	}
	if len(h.pins.abandoned()) != 0 {
		t.Errorf("abandoned outside pinning")
	}
}

func TestHandleInterruptedWhilePinning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		finalState  core.State
		leaseLost   bool
		shutdown    bool
		wantErr     error
		wantAbandon bool
	}{
		{name: "cancelled during the pin", finalState: core.StateCancelled, wantAbandon: true},
		{name: "lease lost and cancelled", finalState: core.StateCancelled, leaseLost: true, wantErr: context.Canceled, wantAbandon: true},
		{name: "lease lost but still pinning", finalState: core.StatePinning, leaseLost: true, wantErr: context.Canceled},
		{name: "shutdown while cancelled", finalState: core.StateCancelled, shutdown: true, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			d := h.store.add(in(core.StatePinning, deploy()))
			h.pins.pinFn = func(ctx context.Context, _ int, _ engine.PinRequest) (engine.PinResult, error) {
				h.store.setState(d.ID, tt.finalState)
				switch {
				case tt.leaseLost:
					cancel(core.ErrLeaseLost)
					return engine.PinResult{}, ctx.Err()
				case tt.shutdown:
					cancel(nil)
					return engine.PinResult{}, ctx.Err()
				}
				return engine.PinResult{Commit: core.Commit{Repository: gitopsRepo, SHA: pinSHA}, Changed: true}, nil
			}

			_, err := h.run(t, ctx, d.ID)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("Handle error = %v, want %v", err, tt.wantErr)
			}
			if got := len(h.pins.abandoned()) == 1; got != tt.wantAbandon {
				t.Errorf("abandoned = %+v, want abandon %t", h.pins.abandoned(), tt.wantAbandon)
			}
			if tt.wantAbandon && h.pins.abandoned()[0].Digest != digestNew {
				t.Errorf("abandon request = %+v", h.pins.abandoned()[0])
			}
			if len(h.notifier.kinds()) != 0 {
				t.Errorf("notifications = %v, want none", h.notifier.kinds())
			}
		})
	}
}

func verifyingRollback(h *harness) core.Deployment {
	bad := h.store.add(in(core.StateCancelled, deploy()))
	rb := deploy()
	rb.Kind = core.KindRollback
	rb.Digest = digestOld
	rb.RollbackOfID = &bad.ID
	return h.store.add(in(core.StateVerifying, rb))
}

func repin(_ context.Context, _ int, req engine.PinRequest) (engine.PinResult, error) {
	if !req.Repin {
		return engine.PinResult{}, errors.New("verify must allow a re-pin")
	}
	return engine.PinResult{Commit: core.Commit{Repository: gitopsRepo, SHA: repinSHA}, Previous: digestNew, Changed: true}, nil
}

func TestHandleRollbackRepinsAfterLatePin(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := verifyingRollback(h)
	h.pins.pinFn = repin

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := h.store.deployment(d.ID)
	if got.State != core.StateSucceeded || got.PinCommitSHA != repinSHA || got.ArgoCDRevision != argoSHA {
		t.Fatalf("after re-pin: %s pin %s revision %s", got.State, got.PinCommitSHA, got.ArgoCDRevision)
	}
	if waits := h.sync.waitCalls(); len(waits) != 1 || waits[0].revision != repinSHA {
		t.Errorf("WaitSynced calls = %+v, want one for the re-pin", waits)
	}
	if re := h.gates.reevaluations(); len(re) != 1 || re[0].PinCommitSHA != repinSHA {
		t.Errorf("Reevaluate calls = %+v, want one for the new pin commit", re)
	}
	if got := h.gates.evaluated(); !slices.Equal(got, []core.GatePhase{core.GatePhaseAfter, core.GatePhaseAfter}) {
		t.Errorf("Evaluate phases = %v, want after gates before and after the re-pin", got)
	}
	if got := len(h.pins.pinRequests()); got != 2 {
		t.Errorf("pins = %d, want the re-pin and its confirmation", got)
	}
	if got := h.notifier.kinds(); !slices.Equal(got, []core.NotificationKind{core.NotificationDeployRolledBack}) {
		t.Errorf("notifications = %v", got)
	}
}

func TestHandleRollbackRepinRunsStickyGatesAgain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rerun      core.GateOutcome
		wantState  core.State
		wantResult engine.Result
		wantReason string
	}{
		{name: "new pin healthy", rerun: core.GateOutcome{Status: core.GatePassed}, wantState: core.StateSucceeded},
		{name: "new pin still settling", rerun: pending(), wantState: core.StateVerifying, wantResult: engine.Result{RequeueAfter: engine.DefaultVerifyPollInterval}},
		{name: "new pin degraded", rerun: failedGate("health", "degraded"), wantState: core.StateFailed, wantReason: "after gates failed: health: degraded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			d := verifyingRollback(h)
			h.gates.rerun = &tt.rerun
			h.pins.pinFn = repin

			result, err := h.run(t, t.Context(), d.ID)
			if err != nil || result != tt.wantResult {
				t.Fatalf("Handle = %+v, %v, want %+v", result, err, tt.wantResult)
			}
			got := h.store.deployment(d.ID)
			if got.State != tt.wantState || !strings.Contains(got.FailureReason, tt.wantReason) {
				t.Fatalf("deployment = %s %q, want %s %q", got.State, got.FailureReason, tt.wantState, tt.wantReason)
			}
			if len(h.gates.reevaluations()) != 1 {
				t.Errorf("Reevaluate calls = %d, want 1", len(h.gates.reevaluations()))
			}
		})
	}
}

func TestHandleRollbackConfirmsUnrecordedRepin(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := verifyingRollback(h)
	h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
		return engine.PinResult{Commit: core.Commit{Repository: gitopsRepo, SHA: repinSHA}, Previous: digestOld}, nil
	}

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.store.deployment(d.ID); got.State != core.StateSucceeded || got.PinCommitSHA != repinSHA {
		t.Fatalf("deployment = %s pin %s, want succeeded with the re-pin recorded", got.State, got.PinCommitSHA)
	}
	if len(h.sync.waitCalls()) != 1 || len(h.gates.reevaluations()) != 1 {
		t.Errorf("a re-pin written by an interrupted claim was not confirmed: waits %+v, reevaluations %d", h.sync.waitCalls(), len(h.gates.reevaluations()))
	}
}

func TestHandleRollbackWithoutPinCommitSucceeds(t *testing.T) {
	t.Parallel()

	h := newHarness()
	rb := verifyingRollback(h)
	h.store.mu.Lock()
	rb.PinCommitSHA = ""
	h.store.deployments[rb.ID] = rb
	h.store.mu.Unlock()
	h.pins.pinFn = func(context.Context, int, engine.PinRequest) (engine.PinResult, error) {
		return engine.PinResult{Previous: digestOld}, nil
	}

	if _, err := h.run(t, t.Context(), rb.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := h.store.deployment(rb.ID); got.State != core.StateSucceeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
	if len(h.sync.waitCalls()) != 0 || len(h.gates.reevaluations()) != 0 {
		t.Errorf("waited or re-evaluated without a new pin commit")
	}
}

func TestHandleRollbackRepinFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		pinErr     error
		waitErr    error
		reErr      error
		elapsed    time.Duration
		wantState  core.State
		wantErr    bool
		wantReason string
		wantClass  core.FailureClass
	}{
		{name: "pin refused", pinErr: fmt.Errorf("no such file: %w", engine.ErrPermanent), wantState: core.StateFailed, wantReason: "confirm the rollback pin " + string(digestOld) + ": no such file", wantClass: core.FailureConfiguration},
		{name: "pin unavailable", pinErr: errors.New("github down"), wantState: core.StateVerifying, wantErr: true},
		{name: "pin unavailable past the deadline", pinErr: errors.New("github down"), elapsed: engine.DefaultVerifyTimeout, wantState: core.StateFailed, wantReason: "gave up after 1h0m0s: github down", wantClass: core.FailureTransient},
		{name: "re-pin past the deadline", elapsed: engine.DefaultVerifyTimeout, wantState: core.StateFailed, wantReason: "was not confirmed within 1h0m0s", wantClass: core.FailureTransient},
		{name: "re-pin never syncs", waitErr: fmt.Errorf("%w", core.ErrSyncTimeout), wantState: core.StateFailed, wantReason: "did not sync", wantClass: core.FailureUnknown},
		{name: "re-pin sync unreadable", waitErr: errors.New("watch closed"), wantState: core.StateVerifying, wantErr: true},
		{name: "re-evaluation fails", reErr: errors.New("database down"), wantState: core.StateVerifying, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			d := verifyingRollback(h)
			h.clock.Advance(tt.elapsed)
			h.gates.reErr = tt.reErr
			h.pins.pinFn = func(ctx context.Context, n int, req engine.PinRequest) (engine.PinResult, error) {
				if tt.pinErr != nil {
					return engine.PinResult{}, tt.pinErr
				}
				return repin(ctx, n, req)
			}
			if tt.waitErr != nil {
				h.sync.waitFn = func(context.Context, string) (core.SyncStatus, error) { return core.SyncStatus{}, tt.waitErr }
			}

			_, err := h.run(t, t.Context(), d.ID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Handle error = %v, want error %t", err, tt.wantErr)
			}
			got := h.store.deployment(d.ID)
			if got.State != tt.wantState || !strings.Contains(got.FailureReason, tt.wantReason) {
				t.Fatalf("deployment = %s %q, want %s %q", got.State, got.FailureReason, tt.wantState, tt.wantReason)
			}
			if got.PinCommitSHA != pinSHA {
				t.Errorf("pin commit = %s, want the unconfirmed re-pin left unrecorded", got.PinCommitSHA)
			}
			if tt.wantState != core.StateFailed {
				return
			}
			if f := h.policy.decided(); len(f) != 1 || f[0].Class != tt.wantClass || f[0].Stage != core.StateVerifying {
				t.Errorf("failures = %+v, want one %s failure while verifying", f, tt.wantClass)
			}
			if got := h.metrics.outcomes(); !slices.Equal(got, []engine.Outcome{engine.OutcomeRollbackFailed}) {
				t.Errorf("outcomes = %v, want rollback_failed", got)
			}
			notes := h.notifier.all()
			if len(notes) != 1 || notes[0].Kind != core.NotificationDeployFailed || notes[0].RollbackOf == nil {
				t.Errorf("notifications = %+v, want deploy.failed naming the rolled back deployment", notes)
			}
		})
	}
}

func TestHandleRollbackConfirmationPinIsBounded(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := verifyingRollback(h)
	h.clock.Advance(engine.DefaultVerifyTimeout - 2*time.Minute)
	var got time.Duration
	h.pins.pinFn = func(ctx context.Context, n int, req engine.PinRequest) (engine.PinResult, error) {
		if deadline, ok := ctx.Deadline(); ok {
			got = time.Until(deadline)
		}
		return repin(ctx, n, req)
	}

	if _, err := h.run(t, t.Context(), d.ID); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if diff := 2*time.Minute - got; diff < 0 || diff > 5*time.Second {
		t.Errorf("Pin got %s, want about the 2m0s left of the verify deadline", got)
	}
}

func TestHandleRollbackRepinWaitIsBounded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{name: "sync timeout", elapsed: 0, want: syncTimeout},
		{name: "near the verify deadline", elapsed: engine.DefaultVerifyTimeout - 3*time.Minute, want: 3 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			d := verifyingRollback(h)
			h.clock.Advance(tt.elapsed)
			h.pins.pinFn = repin
			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			waits := h.sync.waitCalls()
			if len(waits) != 1 {
				t.Fatalf("WaitSynced calls = %d", len(waits))
			}
			if diff := tt.want - waits[0].timeout; diff < 0 || diff > 5*time.Second {
				t.Errorf("WaitSynced got %s to wait, want about %s", waits[0].timeout, tt.want)
			}
		})
	}
}

func TestHandleAnnouncesSuperseded(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := h.store.add(deploy())
	older := in(core.StateSuperseded, deploy())
	older.SupersededByID = &d.ID
	older.StartedAt = nil
	first := h.store.add(older)
	second := h.store.add(older)
	h.store.mu.Lock()
	for _, id := range []core.DeploymentID{first.ID, second.ID} {
		s := h.store.deployments[id]
		s.StartedAt = nil
		h.store.deployments[id] = s
	}
	h.store.mu.Unlock()
	h.gates.set(core.GatePhaseBefore, pending())

	if _, err := h.run(t, t.Context(), d.ID, first.ID, second.ID, 424242); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	notes := h.notifier.all()
	if len(notes) != 2 {
		t.Fatalf("notifications = %v, want two superseded", h.notifier.kinds())
	}
	for i, id := range []core.DeploymentID{first.ID, second.ID} {
		if notes[i].Kind != core.NotificationDeploySuperseded || notes[i].Deployment.ID != id || !strings.Contains(notes[i].Message, fmt.Sprint(d.ID)) {
			t.Errorf("notification %d = %s for %d %q", i, notes[i].Kind, notes[i].Deployment.ID, notes[i].Message)
		}
	}
	h.metrics.mu.Lock()
	defer h.metrics.mu.Unlock()
	for _, f := range h.metrics.finished {
		if f.outcome != engine.OutcomeSuperseded || f.duration != 0 {
			t.Errorf("finished = %+v, want superseded with no duration", f)
		}
	}
	if len(h.metrics.finished) != 2 {
		t.Errorf("finished metrics = %d, want 2", len(h.metrics.finished))
	}
}

func TestQueued(t *testing.T) {
	t.Parallel()

	h := newHarness()
	d := h.store.add(deploy())
	e := h.engine(t)
	e.Queued(t.Context(), d.ID)
	e.Queued(t.Context(), 31337)

	notes := h.notifier.all()
	if len(notes) != 1 || notes[0].Kind != core.NotificationDeployQueued || notes[0].Deployment.ID != d.ID || notes[0].Actor != "octocat" {
		t.Fatalf("notifications = %+v", notes)
	}
	if !notes[0].OccurredAt.Equal(epoch) {
		t.Errorf("occurred at %v, want the injected clock", notes[0].OccurredAt)
	}
}

func TestCancelled(t *testing.T) {
	t.Parallel()

	h := newHarness()
	started := h.store.add(in(core.StatePinning, deploy()))
	h.clock.Advance(time.Minute)
	h.store.setState(started.ID, core.StateCancelled)
	queued := h.store.add(deploy())
	h.store.setState(queued.ID, core.StateCancelled)
	running := h.store.add(in(core.StateSyncing, deploy()))
	unresolved := h.store.add(in(core.StateResolving, deploy()))
	h.store.setState(unresolved.ID, core.StateCancelled)

	h.engine(t).Cancelled(t.Context(), started.ID, queued.ID, running.ID, unresolved.ID, 9999)

	if got := h.notifier.kinds(); !slices.Equal(got, []core.NotificationKind{core.NotificationDeployCancelled, core.NotificationDeployCancelled, core.NotificationDeployCancelled}) {
		t.Errorf("notifications = %v, want three cancellations", got)
	}
	abandoned := h.pins.abandoned()
	if len(abandoned) != 1 || abandoned[0].Deployment.ID != started.ID || abandoned[0].Digest != digestNew {
		t.Errorf("abandoned = %+v, want only the started deployment with a digest", abandoned)
	}
	h.metrics.mu.Lock()
	defer h.metrics.mu.Unlock()
	if len(h.metrics.finished) != 3 || h.metrics.finished[0].duration != time.Minute || h.metrics.finished[1].duration != 0 {
		t.Errorf("finished metrics = %+v", h.metrics.finished)
	}
}

func TestFailurePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		action       engine.FailureAction
		err          error
		wantDecision engine.DecisionReason
	}{
		{name: "fail", action: engine.FailureActionFail, wantDecision: engine.DecisionNotRetryable},
		{name: "unknown action", action: "explode", wantDecision: engine.DecisionNotRetryable},
		{name: "retry outside syncing", action: engine.FailureActionRetry, wantDecision: engine.DecisionNotRetryable},
		{name: "policy error", err: errors.New("policy unavailable"), wantDecision: engine.DecisionNotRetryable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			h.policy.action = tt.action
			h.policy.err = tt.err
			h.gates.set(core.GatePhaseAfter, failedGate("health", "degraded"))
			d := h.store.add(in(core.StateVerifying, deploy()))

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := h.store.deployment(d.ID); got.State != core.StateFailed {
				t.Fatalf("state = %s, want failed", got.State)
			}
			decided := h.policy.decided()
			if len(decided) != 1 || decided[0].Gates == nil || decided[0].Gates.Status != core.GateFailed || decided[0].Stage != core.StateVerifying || decided[0].Attempt != 1 {
				t.Errorf("policy saw %+v", decided)
			}
			data := lastEventData(t, h)
			if data.Decision != tt.wantDecision || data.Attempts != 1 || !data.RollbackEligible || data.FailureClass != core.FailureVersion {
				t.Errorf("failure data = %+v, want decision %s, one attempt, eligible for rollback", data, tt.wantDecision)
			}
		})
	}
}

func TestFailPolicyAlwaysFails(t *testing.T) {
	t.Parallel()

	decision, err := engine.FailPolicy{}.Decide(t.Context(), engine.Failure{Class: core.FailureTransient})
	if err != nil || decision.Action != engine.FailureActionFail || decision.Reason != engine.DecisionNotRetryable || decision.Class != core.FailureTransient {
		t.Fatalf("Decide = %+v, %v", decision, err)
	}
}

func TestFailureReasonIsCleaned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "multi-line", message: "line one\n\tline two\r\n", want: "line one line two"},
		{name: "control characters", message: "bad\x00value\x1b[31m", want: "bad value [31m"},
		{name: "long", message: strings.Repeat("é", 600), want: strings.Repeat("é", 499) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness()
			d := h.store.add(deploy())
			h.gates.set(core.GatePhaseBefore, failedGate("x", tt.message))

			if _, err := h.run(t, t.Context(), d.ID); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := h.store.deployment(d.ID).FailureReason
			want := "before gates failed: x: " + tt.want
			if tt.name == "long" {
				if !strings.HasSuffix(got, "…") || len([]rune(got)) != 500 {
					t.Errorf("reason has %d runes, want 500 ending in an ellipsis", len([]rune(got)))
				}
				return
			}
			if got != want {
				t.Errorf("reason = %q, want %q", got, want)
			}
		})
	}
}
