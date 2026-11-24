package gates_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

var (
	healthyApp     = gates.Application{SyncStatus: gates.SyncSynced, Health: gates.HealthHealthy, Revision: "abc123", OperationPhase: gates.OperationSucceeded}
	progressingApp = gates.Application{SyncStatus: gates.SyncSynced, Health: gates.HealthProgressing, HealthMessage: "rolling out", Revision: "abc123"}
	degradedApp    = gates.Application{SyncStatus: gates.SyncSynced, Health: gates.HealthDegraded, HealthMessage: "CrashLoopBackOff", Revision: "abc123"}
	outOfSyncApp   = gates.Application{SyncStatus: gates.SyncOutOfSync, Health: gates.HealthHealthy, Revision: "abc000"}
	syncingApp     = gates.Application{SyncStatus: gates.SyncSynced, Health: gates.HealthHealthy, Revision: "abc123", OperationPhase: gates.OperationRunning}
)

type appStep struct {
	advance     time.Duration
	app         gates.Application
	err         error
	wantStatus  core.GateStatus
	wantMessage string
	wantFields  map[string]any
}

func runAppSteps(t *testing.T, f *builtinFakes, g gates.Gate, steps []appStep) {
	t.Helper()
	d := deployment(core.KindDeploy)
	d.PinCommitSHA = "fedcba9876543210fedcba9876543210fedcba98"
	var previous *core.GateResult
	for i, step := range steps {
		f.clock.Advance(step.advance)
		f.apps.set(step.app, step.err)
		ev := evaluateGate(t, g, d, previous)
		if ev.Status != step.wantStatus || !strings.Contains(ev.Message, step.wantMessage) {
			t.Fatalf("step %d: Evaluate = %s %q, want %s containing %q", i, ev.Status, ev.Message, step.wantStatus, step.wantMessage)
		}
		evidence := evidenceMap(t, ev)
		for k, want := range step.wantFields {
			if got := evidence[k]; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("step %d: evidence %s = %v, want %v (%s)", i, k, got, want, ev.Evidence)
			}
		}
		previous = resultOf(ev)
	}
}

func TestHealthGate(t *testing.T) {
	t.Parallel()

	start := "2026-11-23T12:00:00Z"
	tests := []struct {
		name  string
		steps []appStep
	}{
		{
			name: "healthy passes",
			steps: []appStep{{
				app: healthyApp, wantStatus: core.GatePassed, wantMessage: "Argo CD Application argocd/web-prod is synced and healthy",
				wantFields: map[string]any{
					"application": "argocd/web-prod", "found": true, "sync": "Synced", "health": "Healthy", "revision": "abc123",
					"pin_commit": "fedcba9876543210fedcba9876543210fedcba98", "degraded_since": nil,
				},
			}},
		},
		{
			name: "progressing then healthy",
			steps: []appStep{
				{app: progressingApp, wantStatus: core.GatePending, wantMessage: "waiting for Argo CD Application argocd/web-prod: sync Synced, health Progressing: rolling out"},
				{advance: time.Minute, app: healthyApp, wantStatus: core.GatePassed},
			},
		},
		{
			name:  "out of sync waits",
			steps: []appStep{{app: outOfSyncApp, wantStatus: core.GatePending, wantMessage: "sync OutOfSync, health Healthy"}},
		},
		{
			name:  "running operation waits",
			steps: []appStep{{app: syncingApp, wantStatus: core.GatePending, wantMessage: "operation running"}},
		},
		{
			name:  "empty status waits",
			steps: []appStep{{app: gates.Application{}, wantStatus: core.GatePending, wantMessage: "sync unknown, health unknown"}},
		},
		{
			name: "suspended and missing wait",
			steps: []appStep{
				{app: gates.Application{SyncStatus: gates.SyncSynced, Health: gates.HealthSuspended}, wantStatus: core.GatePending, wantMessage: "health Suspended"},
				{app: gates.Application{SyncStatus: gates.SyncOutOfSync, Health: gates.HealthMissing}, wantStatus: core.GatePending, wantMessage: "health Missing"},
			},
		},
		{
			name: "application not found waits",
			steps: []appStep{{
				err: fmt.Errorf("get: %w", gates.ErrApplicationNotFound), wantStatus: core.GatePending,
				wantMessage: "Argo CD Application argocd/web-prod not found", wantFields: map[string]any{"found": false},
			}},
		},
		{
			name: "degraded fails after the grace",
			steps: []appStep{
				{app: degradedApp, wantStatus: core.GatePending, wantMessage: "Argo CD Application argocd/web-prod is degraded: CrashLoopBackOff", wantFields: map[string]any{"degraded_since": start}},
				{advance: 59 * time.Second, app: degradedApp, wantStatus: core.GatePending, wantFields: map[string]any{"degraded_since": start}},
				{advance: time.Second, app: degradedApp, wantStatus: core.GateFailed, wantMessage: "Argo CD Application argocd/web-prod stayed degraded for 1m0s: CrashLoopBackOff", wantFields: map[string]any{"degraded_since": start}},
			},
		},
		{
			name: "recovering inside the grace resets it",
			steps: []appStep{
				{app: degradedApp, wantStatus: core.GatePending},
				{advance: 50 * time.Second, app: progressingApp, wantStatus: core.GatePending, wantFields: map[string]any{"degraded_since": nil}},
				{advance: 50 * time.Second, app: degradedApp, wantStatus: core.GatePending, wantFields: map[string]any{"degraded_since": "2026-11-23T12:01:40Z"}},
				{advance: 50 * time.Second, app: degradedApp, wantStatus: core.GatePending},
				{advance: 10 * time.Second, app: degradedApp, wantStatus: core.GateFailed},
			},
		},
		{
			name: "degraded then healthy passes",
			steps: []appStep{
				{app: degradedApp, wantStatus: core.GatePending},
				{advance: 30 * time.Second, app: healthyApp, wantStatus: core.GatePassed},
			},
		},
		{
			name: "not found keeps no degraded state",
			steps: []appStep{
				{app: degradedApp, wantStatus: core.GatePending},
				{advance: 30 * time.Second, err: gates.ErrApplicationNotFound, wantStatus: core.GatePending},
				{advance: 40 * time.Second, app: degradedApp, wantStatus: core.GatePending, wantFields: map[string]any{"degraded_since": "2026-11-23T12:01:10Z"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateHealth})
			runAppSteps(t, f, g, tt.steps)
			if f.apps.namespace != "argocd" || f.apps.name != "web-prod" {
				t.Errorf("read Application %s/%s, want argocd/web-prod", f.apps.namespace, f.apps.name)
			}
		})
	}
}

func TestHealthGateDegradedState(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 11, 23, 11, 58, 0, 0, time.UTC)
	tests := []struct {
		name     string
		previous *core.GateResult
		want     core.GateStatus
	}{
		{name: "no previous result", want: core.GatePending},
		{name: "pending previous past the grace", previous: &core.GateResult{Status: core.GatePending, Evidence: degradedSince(since)}, want: core.GateFailed},
		{name: "terminal previous is ignored", previous: &core.GateResult{Status: core.GateFailed, Evidence: degradedSince(since)}, want: core.GatePending},
		{name: "unparsable evidence starts fresh", previous: &core.GateResult{Status: core.GatePending, Evidence: json.RawMessage(`{"degraded_since":42}`)}, want: core.GatePending},
		{name: "bad timestamp starts fresh", previous: &core.GateResult{Status: core.GatePending, Evidence: json.RawMessage(`{"degraded_since":"yesterday"}`)}, want: core.GatePending},
		{name: "future timestamp starts fresh", previous: &core.GateResult{Status: core.GatePending, Evidence: degradedSince(since.Add(time.Hour))}, want: core.GatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			f.apps.set(degradedApp, nil)
			g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateHealth})
			if ev := evaluateGate(t, g, deployment(core.KindDeploy), tt.previous); ev.Status != tt.want {
				t.Errorf("Evaluate = %s %q, want %s", ev.Status, ev.Message, tt.want)
			}
		})
	}
}

func degradedSince(t time.Time) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"degraded_since":%q}`, t.Format(time.RFC3339)))
}

func TestHealthGateCustomGrace(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	deps := f.deps()
	deps.DegradedGrace = 5 * time.Second
	g := builtinGate(t, deps, webApp(), config.Gate{Type: config.GateHealth})
	runAppSteps(t, f, g, []appStep{
		{app: degradedApp, wantStatus: core.GatePending},
		{advance: 5 * time.Second, app: degradedApp, wantStatus: core.GateFailed, wantMessage: "stayed degraded for 5s"},
	})
}

func TestHealthGateReadError(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	boom := errors.New("forbidden")
	f.apps.set(gates.Application{}, boom)
	g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateHealth})
	_, err := g.Evaluate(t.Context(), gates.Request{Deployment: deployment(core.KindDeploy), Phase: core.GatePhaseAfter})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "argocd/web-prod") {
		t.Errorf("Evaluate error = %v, want %v naming the Application", err, boom)
	}
}

func TestHealthGateCleansApplicationStrings(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.apps.set(gates.Application{SyncStatus: gates.SyncSynced, Health: gates.HealthProgressing, HealthMessage: "bad\x00" + strings.Repeat("m", 5000)}, nil)
	g := builtinGate(t, f.deps(), webApp(), config.Gate{Type: config.GateHealth})
	ev := evaluateGate(t, g, deployment(core.KindDeploy), nil)
	msg, _ := evidenceMap(t, ev)["health_message"].(string)
	if strings.Contains(msg, "\x00") || len(msg) > core.MaxGateMessageBytes {
		t.Errorf("health_message not cleaned: %d bytes", len(msg))
	}
}
