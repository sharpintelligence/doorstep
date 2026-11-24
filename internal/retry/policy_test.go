package retry_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
	"github.com/sharpintelligence/doorstep/internal/retry"
)

type fakeDiagnostics struct {
	mu    sync.Mutex
	pods  []core.PodStatus
	err   error
	calls int
}

func (f *fakeDiagnostics) Pods(ctx context.Context, _ core.Deployment) ([]core.PodStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("no deadline")
	}
	return f.pods, f.err
}

type fakeHistory struct {
	earlier, recent []json.RawMessage
	err             error
}

func (f fakeHistory) Diagnoses(context.Context, core.DeploymentID) ([]json.RawMessage, []json.RawMessage, error) {
	return f.earlier, f.recent, f.err
}

type fakeQueue struct {
	mu      sync.Mutex
	waiting bool
	err     error
	asked   []core.DeploymentID
}

func (f *fakeQueue) Waiting(_ context.Context, _ core.Target, except core.DeploymentID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, except)
	return f.waiting, f.err
}

func defaultSettings() retry.Settings {
	return retry.Settings{Enabled: true, MaxAttempts: 3, Backoff: time.Minute, MaxBackoff: 10 * time.Minute}
}

func TestSettingsDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings retry.Settings
		attempt  int
		want     time.Duration
	}{
		{name: "first", settings: defaultSettings(), attempt: 1, want: time.Minute},
		{name: "second doubles", settings: defaultSettings(), attempt: 2, want: 2 * time.Minute},
		{name: "fourth", settings: defaultSettings(), attempt: 4, want: 8 * time.Minute},
		{name: "capped", settings: defaultSettings(), attempt: 5, want: 10 * time.Minute},
		{name: "no overflow", settings: defaultSettings(), attempt: 200, want: 10 * time.Minute},
		{name: "zero attempt is the first", settings: defaultSettings(), attempt: 0, want: time.Minute},
		{name: "no pause", settings: retry.Settings{MaxBackoff: time.Minute}, attempt: 3, want: 0},
		{name: "fixed", settings: retry.Settings{Backoff: 30 * time.Second, MaxBackoff: 30 * time.Second}, attempt: 3, want: 30 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.settings.Delay(tt.attempt); got != tt.want {
				t.Errorf("Delay(%d) = %s, want %s", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestPolicyDecide(t *testing.T) {
	t.Parallel()

	imagePull := []core.PodStatus{pod("a", "pi-3", digestNew, waiting("ImagePullBackOff"))}
	crash := []core.PodStatus{pod("a", "pi-1", digestNew, crashing(1))}
	tests := []struct {
		name        string
		failure     engine.Failure
		settings    retry.Settings
		unknownApp  bool
		pods        []core.PodStatus
		podsErr     error
		history     fakeHistory
		waiting     bool
		queueErr    error
		wantAction  engine.FailureAction
		wantReason  engine.DecisionReason
		wantClass   core.FailureClass
		wantDelay   time.Duration
		wantSummary string
		wantQueue   bool
	}{
		{
			name:        "transient failure is retried",
			failure:     degraded("x"),
			settings:    defaultSettings(),
			pods:        imagePull,
			wantAction:  engine.FailureActionRetry,
			wantReason:  engine.DecisionTransient,
			wantClass:   core.FailureTransient,
			wantDelay:   time.Minute,
			wantSummary: "transient: image pull failing on node pi-3",
			wantQueue:   true,
		},
		{
			name:       "second attempt backs off longer",
			failure:    withAttempt(degraded("x"), 2),
			settings:   defaultSettings(),
			pods:       imagePull,
			wantAction: engine.FailureActionRetry,
			wantReason: engine.DecisionTransient,
			wantClass:  core.FailureTransient,
			wantDelay:  2 * time.Minute,
			wantQueue:  true,
		},
		{
			name:       "unknown is retried",
			failure:    degraded("application health is degraded"),
			settings:   defaultSettings(),
			wantAction: engine.FailureActionRetry,
			wantReason: engine.DecisionTransient,
			wantClass:  core.FailureUnknown,
			wantDelay:  time.Minute,
			wantQueue:  true,
		},
		{
			name:       "budget spent",
			failure:    withAttempt(degraded("x"), 3),
			settings:   defaultSettings(),
			pods:       imagePull,
			wantAction: engine.FailureActionFail,
			wantReason: engine.DecisionRetriesExhausted,
			wantClass:  core.FailureTransient,
		},
		{
			name:       "version caused fails at once",
			failure:    degraded("x"),
			settings:   defaultSettings(),
			pods:       []core.PodStatus{pod("a", "pi-1", digestNew, crashing(1)), pod("b", "pi-2", digestNew, crashing(1))},
			wantAction: engine.FailureActionFail,
			wantReason: engine.DecisionVersionCaused,
			wantClass:  core.FailureVersion,
		},
		{
			name:       "configuration is not retried",
			failure:    syncFailure(core.SyncReasonNotFound, core.FailureConfiguration, "gone"),
			settings:   defaultSettings(),
			wantAction: engine.FailureActionFail,
			wantReason: engine.DecisionNotRetryable,
			wantClass:  core.FailureConfiguration,
		},
		{
			name:       "retries disabled",
			failure:    degraded("x"),
			settings:   retry.Settings{MaxAttempts: 3, Backoff: time.Minute, MaxBackoff: time.Minute},
			pods:       imagePull,
			wantAction: engine.FailureActionFail,
			wantReason: engine.DecisionRetriesDisabled,
			wantClass:  core.FailureTransient,
		},
		{
			name:       "unknown app is not retried",
			failure:    degraded("x"),
			unknownApp: true,
			pods:       imagePull,
			wantAction: engine.FailureActionFail,
			wantReason: engine.DecisionRetriesDisabled,
			wantClass:  core.FailureTransient,
		},
		{
			name:       "a newer deployment is waiting",
			failure:    degraded("x"),
			settings:   defaultSettings(),
			pods:       imagePull,
			waiting:    true,
			wantAction: engine.FailureActionFail,
			wantReason: engine.DecisionNewerReleaseQueued,
			wantClass:  core.FailureTransient,
			wantQueue:  true,
		},
		{
			name:       "queue error does not block a retry",
			failure:    degraded("x"),
			settings:   defaultSettings(),
			pods:       imagePull,
			queueErr:   errors.New("connection reset"),
			wantAction: engine.FailureActionRetry,
			wantReason: engine.DecisionTransient,
			wantClass:  core.FailureTransient,
			wantDelay:  time.Minute,
			wantQueue:  true,
		},
		{
			name:       "pod read error falls back to the Application",
			failure:    degraded("Deployment blog: ImagePullBackOff"),
			settings:   defaultSettings(),
			podsErr:    errors.New("pods is forbidden"),
			wantAction: engine.FailureActionRetry,
			wantReason: engine.DecisionTransient,
			wantClass:  core.FailureTransient,
			wantDelay:  time.Minute,
			wantQueue:  true,
		},
		{
			name:     "earlier attempts supply the evidence",
			failure:  withAttempt(degraded("x"), 2),
			settings: defaultSettings(),
			pods:     []core.PodStatus{pod("a", "pi-2", digestNew, crashing(1))},
			history: fakeHistory{earlier: []json.RawMessage{
				json.RawMessage(`not json`),
				json.RawMessage(`{"verdict":"unknown","findings":[{"signal":"crash_loop","node":"pi-1","pods":1,"reason":"Error","exit_code":1}]}`),
			}},
			wantAction: engine.FailureActionFail,
			wantReason: engine.DecisionVersionCaused,
			wantClass:  core.FailureVersion,
		},
		{
			name:       "recent failures of other versions point at the node",
			failure:    degraded("x"),
			settings:   defaultSettings(),
			pods:       crash,
			history:    fakeHistory{recent: []json.RawMessage{json.RawMessage(`{"verdict":"unknown","digest":"sha256:3333333333333333333333333333333333333333333333333333333333333333","nodes":["pi-1"],"findings":[{"signal":"crash_loop","node":"pi-1","pods":1}]}`)}},
			wantAction: engine.FailureActionRetry,
			wantReason: engine.DecisionTransient,
			wantClass:  core.FailureTransient,
			wantDelay:  time.Minute,
			wantQueue:  true,
		},
		{
			name:       "history error classifies without it",
			failure:    degraded("x"),
			settings:   defaultSettings(),
			pods:       crash,
			history:    fakeHistory{err: errors.New("timeout"), recent: []json.RawMessage{json.RawMessage(`{"nodes":["pi-1"]}`)}},
			wantAction: engine.FailureActionRetry,
			wantReason: engine.DecisionTransient,
			wantClass:  core.FailureUnknown,
			wantDelay:  time.Minute,
			wantQueue:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			diagnostics := &fakeDiagnostics{pods: tt.pods, err: tt.podsErr}
			queue := &fakeQueue{waiting: tt.waiting, err: tt.queueErr}
			p, err := retry.New(retry.Options{
				Settings: func(core.Target) (retry.Settings, bool) {
					return tt.settings, !tt.unknownApp
				},
				Diagnostics: diagnostics,
				History:     tt.history,
				Queue:       queue,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := p.Decide(t.Context(), tt.failure)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if got.Action != tt.wantAction || got.Reason != tt.wantReason || got.Class != tt.wantClass || got.Delay != tt.wantDelay {
				t.Errorf("Decide = %s %s %s %s, want %s %s %s %s", got.Action, got.Reason, got.Class, got.Delay, tt.wantAction, tt.wantReason, tt.wantClass, tt.wantDelay)
			}
			if tt.wantSummary != "" && got.Summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", got.Summary, tt.wantSummary)
			}
			var diagnosis retry.Diagnosis
			if err := json.Unmarshal(got.Diagnosis, &diagnosis); err != nil || diagnosis.Verdict != got.Class || diagnosis.Summary != got.Summary || diagnosis.Attempt != tt.failure.Attempt {
				t.Errorf("diagnosis = %s (%v)", got.Diagnosis, err)
			}
			if diagnosis.PodsInspected != (tt.podsErr == nil) {
				t.Errorf("pods inspected = %t, pod error %v", diagnosis.PodsInspected, tt.podsErr)
			}
			if !tt.unknownApp && got.MaxAttempts != tt.settings.MaxAttempts {
				t.Errorf("max attempts = %d, want %d", got.MaxAttempts, tt.settings.MaxAttempts)
			}
			if asked := len(queue.asked) > 0; asked != tt.wantQueue {
				t.Errorf("queue consulted = %t, want %t", asked, tt.wantQueue)
			}
			if diagnostics.calls != 1 {
				t.Errorf("pod reads = %d, want 1", diagnostics.calls)
			}
		})
	}
}

func withAttempt(f engine.Failure, attempt int) engine.Failure {
	f.Attempt = attempt
	return f
}

func TestPolicyLeavesOtherFailuresAlone(t *testing.T) {
	t.Parallel()

	diagnostics := &fakeDiagnostics{}
	p, err := retry.New(retry.Options{
		Settings:    func(core.Target) (retry.Settings, bool) { return defaultSettings(), true },
		Diagnostics: diagnostics,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	failures := []engine.Failure{
		{Deployment: deployment(), Stage: core.StateVerifying, Class: core.FailureVersion, Gates: &core.GateOutcome{Status: core.GateFailed}},
		{Deployment: deployment(), Stage: core.StatePinning, Class: core.FailureTransient},
		{Deployment: deployment(), Stage: core.StateSyncing, Class: core.FailureConfiguration},
	}
	for _, f := range failures {
		got, err := p.Decide(t.Context(), f)
		if err != nil || got.Action != engine.FailureActionFail || got.Reason != engine.DecisionNotRetryable || got.Class != f.Class || got.Diagnosis != nil {
			t.Errorf("Decide(%s) = %+v, %v", f.Stage, got, err)
		}
	}
	if diagnostics.calls != 0 {
		t.Errorf("pod reads = %d, want none", diagnostics.calls)
	}
}

func TestPolicyWithoutOptionalPorts(t *testing.T) {
	t.Parallel()

	p, err := retry.New(retry.Options{Settings: func(core.Target) (retry.Settings, bool) { return defaultSettings(), true }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := p.Decide(t.Context(), degraded("Back-off pulling image: ErrImagePull"))
	if err != nil || got.Action != engine.FailureActionRetry || got.Class != core.FailureTransient {
		t.Fatalf("Decide = %+v, %v", got, err)
	}
	if _, err := retry.New(retry.Options{}); err == nil {
		t.Error("New without settings succeeded")
	}
}
