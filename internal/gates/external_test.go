package gates_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

var reportedAt = time.Date(2026, 11, 23, 12, 5, 0, 0, time.UTC)

type fakeReports struct {
	mu      sync.Mutex
	reports map[string]core.GateReport
	err     error
	reads   []string
}

func newFakeReports() *fakeReports {
	return &fakeReports{reports: map[string]core.GateReport{}}
}

func (f *fakeReports) set(r core.GateReport) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports[key(r.DeploymentID, r.Gate)] = r
}

func (f *fakeReports) GetGateReport(_ context.Context, id core.DeploymentID, gate string) (core.GateReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, key(id, gate))
	if f.err != nil {
		return core.GateReport{}, f.err
	}
	r, ok := f.reports[key(id, gate)]
	if !ok {
		return core.GateReport{}, core.ErrNotFound
	}
	return r, nil
}

func externalReport(status core.GateStatus, message string) core.GateReport {
	return core.GateReport{
		DeploymentID: 7,
		Gate:         "e2e",
		Status:       status,
		Message:      message,
		URL:          "https://ci.example.com/runs/9",
		Details:      json.RawMessage(`{"passed": 12}`),
		ReportedBy:   "token:ci",
		CreatedAt:    reportedAt.Add(-time.Minute),
		UpdatedAt:    reportedAt,
	}
}

func externalPlan(t *testing.T, reports gates.ExternalReports, g config.Gate) gates.Plan {
	t.Helper()
	r := gates.NewRegistry()
	if err := gates.RegisterExternal(r, reports); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	app := webApp()
	app.Gates = config.Gates{Before: []config.Gate{}, After: []config.Gate{g}}
	plan, err := r.Plan(app)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

func TestRegisterExternal(t *testing.T) {
	t.Parallel()

	if err := gates.RegisterExternal(nil, newFakeReports()); err == nil {
		t.Error("nil registry accepted")
	}
	if err := gates.RegisterExternal(gates.NewRegistry(), nil); err == nil {
		t.Error("nil report reader accepted")
	}
	r := gates.NewRegistry()
	if err := gates.RegisterExternal(r, newFakeReports()); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	if err := gates.RegisterExternal(r, newFakeReports()); !errors.Is(err, gates.ErrDuplicateGateType) {
		t.Errorf("second registration = %v, want ErrDuplicateGateType", err)
	}

	plan := externalPlan(t, newFakeReports(), config.Gate{Type: config.GateExternal, Name: "e2e", Timeout: new(config.Duration(time.Hour))})
	b := plan.After[0]
	if b.Name != "e2e" || b.Type != config.GateExternal || b.Kind != metrics.GateKindExternal || b.Phase != core.GatePhaseAfter ||
		b.SkipOnRollback || b.Timeout != time.Hour {
		t.Errorf("binding = %+v", b)
	}
	untimed := externalPlan(t, newFakeReports(), config.Gate{Type: config.GateExternal, Name: "e2e"})
	if untimed.After[0].Timeout != 0 {
		t.Errorf("timeout = %s, want none", untimed.After[0].Timeout)
	}
}

func TestExternalGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		report           *core.GateReport
		kind             core.Kind
		applyToRollbacks bool
		readErr          error
		wantStatus       core.GateStatus
		wantMessage      string
		wantEvidence     string
		wantErr          string
		wantReads        int
	}{
		{
			name:         "nothing reported",
			wantStatus:   core.GatePending,
			wantMessage:  "waiting for an external result",
			wantEvidence: `{"reported":false}`,
			wantReads:    1,
		},
		{
			name:         "pending progress",
			report:       new(externalReport(core.GatePending, "3 of 10 suites done")),
			wantStatus:   core.GatePending,
			wantMessage:  "3 of 10 suites done",
			wantEvidence: `{"reported":true,"status":"pending","reported_by":"token:ci","reported_at":"2026-11-23T12:05:00Z","url":"https://ci.example.com/runs/9","details":{"passed":12}}`,
			wantReads:    1,
		},
		{
			name:         "passed",
			report:       new(externalReport(core.GatePassed, "")),
			wantStatus:   core.GatePassed,
			wantMessage:  "reported passed by token:ci",
			wantEvidence: `{"reported":true,"status":"passed","reported_by":"token:ci","reported_at":"2026-11-23T12:05:00Z","url":"https://ci.example.com/runs/9","details":{"passed":12}}`,
			wantReads:    1,
		},
		{
			name: "failed without url or details",
			report: func() *core.GateReport {
				r := externalReport(core.GateFailed, "3 tests failed")
				r.URL, r.Details = "", json.RawMessage(`{}`)
				return &r
			}(),
			wantStatus:   core.GateFailed,
			wantMessage:  "3 tests failed",
			wantEvidence: `{"reported":true,"status":"failed","reported_by":"token:ci","reported_at":"2026-11-23T12:05:00Z"}`,
			wantReads:    1,
		},
		{
			name:        "rollback skips without reading",
			report:      new(externalReport(core.GateFailed, "")),
			kind:        core.KindRollback,
			wantStatus:  core.GateSkipped,
			wantMessage: "skipped for rollback",
		},
		{
			name:             "rollback opted in",
			report:           new(externalReport(core.GatePassed, "ok")),
			kind:             core.KindRollback,
			applyToRollbacks: true,
			wantStatus:       core.GatePassed,
			wantMessage:      "ok",
			wantEvidence:     `{"reported":true,"status":"passed","reported_by":"token:ci","reported_at":"2026-11-23T12:05:00Z","url":"https://ci.example.com/runs/9","details":{"passed":12}}`,
			wantReads:        1,
		},
		{
			name:      "read error",
			readErr:   errors.New("connection refused"),
			wantErr:   "read external result of gate e2e: connection refused",
			wantReads: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reports := newFakeReports()
			reports.err = tt.readErr
			if tt.report != nil {
				reports.set(*tt.report)
			}
			plan := externalPlan(t, reports, config.Gate{Type: config.GateExternal, Name: "e2e", ApplyToRollbacks: tt.applyToRollbacks})
			kind := tt.kind
			if kind == "" {
				kind = core.KindDeploy
			}
			ev, err := plan.After[0].Gate.Evaluate(t.Context(), gates.Request{Deployment: deployment(kind), Phase: core.GatePhaseAfter})
			if len(reports.reads) != tt.wantReads {
				t.Errorf("reads = %v, want %d", reports.reads, tt.wantReads)
			}
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("Evaluate error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if ev.Status != tt.wantStatus || ev.Message != tt.wantMessage || string(ev.Evidence) != tt.wantEvidence {
				t.Errorf("Evaluate = %s %q %s, want %s %q %s", ev.Status, ev.Message, ev.Evidence, tt.wantStatus, tt.wantMessage, tt.wantEvidence)
			}
			if reports.reads != nil && reports.reads[0] != "7/e2e" {
				t.Errorf("read %s, want 7/e2e", reports.reads[0])
			}
		})
	}
}

func TestExternalGateCancelledContext(t *testing.T) {
	t.Parallel()

	reports := newFakeReports()
	plan := externalPlan(t, reports, config.Gate{Type: config.GateExternal, Name: "e2e"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := plan.After[0].Gate.Evaluate(ctx, gates.Request{Deployment: deployment(core.KindDeploy)}); !errors.Is(err, context.Canceled) {
		t.Errorf("Evaluate = %v, want context.Canceled", err)
	}
	if len(reports.reads) != 0 {
		t.Errorf("reads = %v, want none", reports.reads)
	}
}

func TestExternalGateWithEvaluator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		steps       func(t *testing.T, h *harness, reports *fakeReports, d core.Deployment) core.GateOutcome
		wantStatus  core.GateStatus
		wantMessage string
		wantObserve []string
	}{
		{
			name: "pending until reported then sticky",
			steps: func(t *testing.T, h *harness, reports *fakeReports, d core.Deployment) core.GateOutcome {
				t.Helper()
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				reports.set(externalReport(core.GatePassed, "green"))
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				reports.set(externalReport(core.GatePending, "rerun"))
				return mustEvaluate(t, h, d, core.GatePhaseAfter)
			},
			wantStatus: core.GatePassed, wantMessage: "green",
			wantObserve: []string{"e2e=pending", "e2e=passed"},
		},
		{
			name: "timeout turns waiting into failed",
			steps: func(t *testing.T, h *harness, _ *fakeReports, d core.Deployment) core.GateOutcome {
				t.Helper()
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				h.clock.Advance(time.Hour)
				return mustEvaluate(t, h, d, core.GatePhaseAfter)
			},
			wantStatus: core.GateFailed, wantMessage: "timed out after 30m0s",
			wantObserve: []string{"e2e=pending", "e2e=failed"},
		},
		{
			name: "a report after the timeout does not revive the gate",
			steps: func(t *testing.T, h *harness, reports *fakeReports, d core.Deployment) core.GateOutcome {
				t.Helper()
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				h.clock.Advance(time.Hour)
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				reports.set(externalReport(core.GatePassed, "late"))
				return mustEvaluate(t, h, d, core.GatePhaseAfter)
			},
			wantStatus: core.GateFailed, wantMessage: "timed out after 30m0s",
			wantObserve: []string{"e2e=pending", "e2e=failed"},
		},
		{
			name: "reevaluate keeps the reported verdict",
			steps: func(t *testing.T, h *harness, reports *fakeReports, d core.Deployment) core.GateOutcome {
				t.Helper()
				reports.set(externalReport(core.GateFailed, "smoke tests failed"))
				mustEvaluate(t, h, d, core.GatePhaseAfter)
				out, err := h.eval.Reevaluate(t.Context(), d, core.GatePhaseAfter)
				if err != nil {
					t.Fatalf("Reevaluate: %v", err)
				}
				return out
			},
			wantStatus: core.GateFailed, wantMessage: "smoke tests failed",
			wantObserve: []string{"e2e=failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reports := newFakeReports()
			plan := externalPlan(t, reports, config.Gate{Type: config.GateExternal, Name: "e2e", Timeout: new(config.Duration(30 * time.Minute))})
			h := newHarness(t, plan)
			out := tt.steps(t, h, reports, deployment(core.KindDeploy))
			if out.Status != tt.wantStatus || out.Results[0].Message != tt.wantMessage {
				t.Errorf("outcome = %s %q, want %s %q", out.Status, out.Results[0].Message, tt.wantStatus, tt.wantMessage)
			}
			if got := strings.Join(h.observer.statuses(), ","); got != strings.Join(tt.wantObserve, ",") {
				t.Errorf("observed %s, want %s", got, strings.Join(tt.wantObserve, ","))
			}
			for _, o := range h.metrics.all() {
				if o.gate != metrics.GateKindExternal {
					t.Errorf("metrics gate kind = %s, want external", o.gate)
				}
			}
		})
	}
}
