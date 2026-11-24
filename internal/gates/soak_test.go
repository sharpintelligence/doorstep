package gates_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

func soakConfig(d time.Duration) config.Gate {
	return config.Gate{Type: config.GateSoak, Duration: config.Duration(d)}
}

func TestSoakGate(t *testing.T) {
	t.Parallel()

	const (
		t0 = "2026-11-23T12:00:00Z"
		t1 = "2026-11-23T12:01:00Z"
	)
	tests := []struct {
		name  string
		steps []appStep
	}{
		{
			name: "passes after the soak",
			steps: []appStep{
				{app: healthyApp, wantStatus: core.GatePending, wantMessage: "soaking until 2026-11-23T12:10:00Z", wantFields: map[string]any{
					"healthy_since": t0, "until": "2026-11-23T12:10:00Z", "soak": "10m0s", "sync": "Synced", "health": "Healthy", "revision": "abc123",
				}},
				{advance: 5 * time.Minute, app: healthyApp, wantStatus: core.GatePending, wantMessage: "soaking until 2026-11-23T12:10:00Z", wantFields: map[string]any{"healthy_since": t0}},
				{advance: 5*time.Minute - time.Second, app: healthyApp, wantStatus: core.GatePending},
				{advance: time.Second, app: healthyApp, wantStatus: core.GatePassed, wantMessage: "healthy for the 10m0s soak"},
			},
		},
		{
			name: "clock starts at the first healthy observation",
			steps: []appStep{
				{app: progressingApp, wantStatus: core.GatePending, wantMessage: "health Progressing", wantFields: map[string]any{"healthy_since": nil, "until": nil}},
				{advance: time.Minute, app: healthyApp, wantStatus: core.GatePending, wantMessage: "soaking until 2026-11-23T12:11:00Z", wantFields: map[string]any{"healthy_since": t1}},
				{advance: 10 * time.Minute, app: healthyApp, wantStatus: core.GatePassed},
			},
		},
		{
			name: "progressing does not reset the clock",
			steps: []appStep{
				{app: healthyApp, wantStatus: core.GatePending},
				{advance: 4 * time.Minute, app: progressingApp, wantStatus: core.GatePending, wantMessage: "health Progressing", wantFields: map[string]any{"healthy_since": t0, "until": "2026-11-23T12:10:00Z"}},
				{advance: 6 * time.Minute, app: healthyApp, wantStatus: core.GatePassed},
			},
		},
		{
			name: "not healthy at the end keeps waiting",
			steps: []appStep{
				{app: healthyApp, wantStatus: core.GatePending},
				{advance: 11 * time.Minute, app: outOfSyncApp, wantStatus: core.GatePending, wantMessage: "sync OutOfSync"},
				{advance: time.Minute, app: syncingApp, wantStatus: core.GatePending, wantMessage: "operation running"},
				{advance: time.Minute, app: healthyApp, wantStatus: core.GatePassed},
			},
		},
		{
			name: "degraded past the grace fails",
			steps: []appStep{
				{app: healthyApp, wantStatus: core.GatePending},
				{advance: 3 * time.Minute, app: degradedApp, wantStatus: core.GatePending, wantMessage: "is degraded: CrashLoopBackOff", wantFields: map[string]any{"degraded_since": "2026-11-23T12:03:00Z", "healthy_since": t0}},
				{advance: time.Minute, app: degradedApp, wantStatus: core.GateFailed, wantMessage: "Argo CD Application argocd/web-prod stayed degraded for 1m0s while soaking: CrashLoopBackOff"},
			},
		},
		{
			name: "brief degradation inside the grace is tolerated",
			steps: []appStep{
				{app: healthyApp, wantStatus: core.GatePending},
				{advance: 3 * time.Minute, app: degradedApp, wantStatus: core.GatePending},
				{advance: 30 * time.Second, app: healthyApp, wantStatus: core.GatePending, wantFields: map[string]any{"healthy_since": t0, "degraded_since": nil}},
				{advance: 6*time.Minute + 30*time.Second, app: healthyApp, wantStatus: core.GatePassed},
			},
		},
		{
			name: "missing application waits and keeps the clock",
			steps: []appStep{
				{app: healthyApp, wantStatus: core.GatePending},
				{advance: 2 * time.Minute, err: gates.ErrApplicationNotFound, wantStatus: core.GatePending, wantMessage: "not found", wantFields: map[string]any{"healthy_since": t0}},
				{advance: 8 * time.Minute, app: healthyApp, wantStatus: core.GatePassed},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			g := builtinGate(t, f.deps(), webApp(), soakConfig(10*time.Minute))
			runAppSteps(t, f, g, tt.steps)
		})
	}
}

func TestSoakGateState(t *testing.T) {
	t.Parallel()

	healthySince := func(s string) json.RawMessage { return json.RawMessage(`{"healthy_since":"` + s + `"}`) }
	tests := []struct {
		name     string
		previous *core.GateResult
		want     core.GateStatus
	}{
		{name: "pending previous carries the clock", previous: &core.GateResult{Status: core.GatePending, Evidence: healthySince("2026-11-23T11:50:00Z")}, want: core.GatePassed},
		{name: "terminal previous restarts the soak", previous: &core.GateResult{Status: core.GatePassed, Evidence: healthySince("2026-11-23T11:50:00Z")}, want: core.GatePending},
		{name: "failed previous restarts the soak", previous: &core.GateResult{Status: core.GateFailed, Evidence: healthySince("2026-11-23T11:50:00Z")}, want: core.GatePending},
		{name: "unparsable evidence starts fresh", previous: &core.GateResult{Status: core.GatePending, Evidence: json.RawMessage(`[1,2]`)}, want: core.GatePending},
		{name: "future clock starts fresh", previous: &core.GateResult{Status: core.GatePending, Evidence: healthySince("2026-11-23T13:00:00Z")}, want: core.GatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBuiltinFakes()
			f.apps.set(healthyApp, nil)
			g := builtinGate(t, f.deps(), webApp(), soakConfig(10*time.Minute))
			ev := evaluateGate(t, g, deployment(core.KindDeploy), tt.previous)
			if ev.Status != tt.want {
				t.Errorf("Evaluate = %s %q, want %s", ev.Status, ev.Message, tt.want)
			}
		})
	}
}

func TestSoakGateReadError(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	boom := errors.New("timeout")
	f.apps.set(gates.Application{}, boom)
	g := builtinGate(t, f.deps(), webApp(), soakConfig(time.Minute))
	if _, err := g.Evaluate(t.Context(), gates.Request{Deployment: deployment(core.KindDeploy), Phase: core.GatePhaseAfter}); !errors.Is(err, boom) {
		t.Errorf("Evaluate error = %v, want %v", err, boom)
	}
}

func TestSoakGateRequiresDuration(t *testing.T) {
	t.Parallel()

	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	app := webApp()
	app.Gates = config.Gates{After: []config.Gate{{Type: config.GateSoak, Name: "soak"}}}
	if _, err := r.Plan(app); err == nil {
		t.Error("Plan accepted a soak gate without a duration")
	}
}

func TestSoakTimesOutThroughTheEvaluator(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.apps.set(progressingApp, nil)
	g := builtinGate(t, f.deps(), webApp(), soakConfig(time.Minute))
	binding := gates.Binding{Name: "soak", Type: config.GateSoak, Kind: metrics.GateKindSoak, Phase: core.GatePhaseAfter, Timeout: 11 * time.Minute, Gate: g}
	h := newHarness(t, gates.Plan{After: []gates.Binding{binding}})
	d := deployment(core.KindDeploy)
	if out := mustEvaluate(t, h, d, core.GatePhaseAfter); out.Status != core.GatePending {
		t.Fatalf("first evaluation = %s, want pending", out.Status)
	}
	h.clock.Advance(11 * time.Minute)
	f.clock.Advance(11 * time.Minute)
	out := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if out.Status != core.GateFailed || out.Results[0].Message != "timed out after 11m0s" {
		t.Errorf("after the timeout = %s %q, want failed by timeout", out.Status, out.Results[0].Message)
	}
}
