package gates_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

const reportSHA = "0123456789abcdef0123456789abcdef01234567"

var ciCaller = core.Actor{Kind: core.ActorToken, ID: "ci"}

type fakeReportStore struct {
	mu          sync.Mutex
	deployments map[core.DeploymentID]core.Deployment
	active      map[string]core.Deployment
	saveErr     error
	unchanged   bool
	saved       []core.GateReport
	audits      []core.AuditEntry
	lookups     []string
}

func (f *fakeReportStore) GetDeployment(_ context.Context, id core.DeploymentID) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deployments[id]
	if !ok {
		return core.Deployment{}, core.ErrNotFound
	}
	return d, nil
}

func (f *fakeReportStore) ActiveDeploymentForCommit(_ context.Context, t core.Target, sha string) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, t.String()+"@"+sha)
	d, ok := f.active[t.App+"@"+sha]
	if !ok {
		return core.Deployment{}, core.ErrNotFound
	}
	return d, nil
}

func (f *fakeReportStore) SaveGateReport(_ context.Context, r core.GateReport, audit core.AuditEntry) (core.GateReport, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return core.GateReport{}, false, f.saveErr
	}
	f.saved = append(f.saved, r)
	f.audits = append(f.audits, audit)
	r.CreatedAt, r.UpdatedAt = reportedAt, reportedAt
	return r, !f.unchanged, nil
}

func reportCatalog() config.Catalog {
	timeout := config.Duration(time.Hour)
	return config.Catalog{Apps: []config.App{
		{
			Name: "web",
			Gates: config.Gates{
				Before: []config.Gate{
					{Type: config.GateChecks, Name: "checks"},
					{Type: config.GateExternal, Name: "approval"},
				},
				After: []config.Gate{
					{Type: config.GateHealth, Name: "health"},
					{Type: config.GateExternal, Name: "e2e", Timeout: &timeout},
					{Type: config.GateExternal, Name: "smoke", ApplyToRollbacks: true},
				},
			},
		},
		{Name: "api"},
	}}
}

func reportDeployment(id core.DeploymentID, app string, kind core.Kind, state core.State) core.Deployment {
	d := core.Deployment{
		ID:     id,
		Target: core.AppTarget(app),
		Kind:   kind,
		State:  state,
		Commit: core.Commit{Repository: "acme/" + app, SHA: reportSHA},
	}
	if kind == core.KindRollback {
		d.RollbackOfID = new(core.DeploymentID(1))
	}
	return d
}

func newReportStore() *fakeReportStore {
	ds := []core.Deployment{
		reportDeployment(1, "web", core.KindDeploy, core.StateVerifying),
		reportDeployment(2, "web", core.KindDeploy, core.StateSyncing),
		reportDeployment(3, "web", core.KindDeploy, core.StateSucceeded),
		reportDeployment(4, "web", core.KindRollback, core.StateVerifying),
		reportDeployment(5, "web", core.KindDeploy, core.StateQueued),
		reportDeployment(6, "removed", core.KindDeploy, core.StateVerifying),
	}
	f := &fakeReportStore{deployments: map[core.DeploymentID]core.Deployment{}, active: map[string]core.Deployment{}}
	for _, d := range ds {
		f.deployments[d.ID] = d
	}
	f.active["web@"+reportSHA] = ds[0]
	return f
}

func TestNewReporter(t *testing.T) {
	t.Parallel()

	if _, err := gates.NewReporter(nil, reportCatalog()); err == nil {
		t.Error("nil store accepted")
	}
}

func TestReporterReport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		req        gates.ReportRequest
		saveErr    error
		unchanged  bool
		wantErr    error
		wantID     core.DeploymentID
		wantPhase  core.GatePhase
		wantSaved  bool
		wantLookup string
	}{
		{
			name:      "after gate while verifying",
			req:       gates.ReportRequest{DeploymentID: 1, Gate: "e2e", Status: core.GatePassed},
			wantID:    1,
			wantPhase: core.GatePhaseAfter,
			wantSaved: true,
		},
		{
			name:      "before gate while queued",
			req:       gates.ReportRequest{DeploymentID: 5, Gate: "approval", Status: core.GatePassed},
			wantID:    5,
			wantPhase: core.GatePhaseBefore,
			wantSaved: true,
		},
		{
			name:      "before gate after it was decided by the phase moving on",
			req:       gates.ReportRequest{DeploymentID: 1, Gate: "approval", Status: core.GatePending},
			wantID:    1,
			wantPhase: core.GatePhaseBefore,
			wantSaved: true,
		},
		{
			name:       "by commit",
			req:        gates.ReportRequest{App: "web", CommitSHA: reportSHA, Gate: "e2e", Status: core.GateFailed, Message: "red"},
			wantID:     1,
			wantPhase:  core.GatePhaseAfter,
			wantSaved:  true,
			wantLookup: "web/" + core.DefaultEnvironment + "@" + reportSHA,
		},
		{
			name:      "unchanged retry",
			req:       gates.ReportRequest{DeploymentID: 1, Gate: "e2e", Status: core.GatePassed},
			unchanged: true,
			wantID:    1,
			wantPhase: core.GatePhaseAfter,
			wantSaved: true,
		},
		{
			name:      "rollback with opt-in",
			req:       gates.ReportRequest{DeploymentID: 4, Gate: "smoke", Status: core.GatePassed},
			wantID:    4,
			wantPhase: core.GatePhaseAfter,
			wantSaved: true,
		},
		{
			name:    "unknown deployment",
			req:     gates.ReportRequest{DeploymentID: 99, Gate: "e2e", Status: core.GatePassed},
			wantErr: core.ErrNotFound,
		},
		{
			name:       "no active deployment for commit",
			req:        gates.ReportRequest{App: "web", CommitSHA: "89abcdef0123456789abcdef0123456789abcdef", Gate: "e2e", Status: core.GatePassed},
			wantErr:    core.ErrNotFound,
			wantLookup: "web/" + core.DefaultEnvironment + "@89abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name:    "unknown app for commit",
			req:     gates.ReportRequest{App: "nope", CommitSHA: reportSHA, Gate: "e2e", Status: core.GatePassed},
			wantErr: core.ErrNotFound,
		},
		{
			name:    "unknown gate",
			req:     gates.ReportRequest{DeploymentID: 1, Gate: "nope", Status: core.GatePassed},
			wantErr: gates.ErrUnknownGate,
		},
		{
			name:    "app without gates",
			req:     gates.ReportRequest{DeploymentID: 6, Gate: "e2e", Status: core.GatePassed},
			wantErr: gates.ErrUnknownGate,
		},
		{
			name:    "built-in gate",
			req:     gates.ReportRequest{DeploymentID: 1, Gate: "health", Status: core.GatePassed},
			wantErr: gates.ErrGateNotExternal,
		},
		{
			name:    "finished deployment",
			req:     gates.ReportRequest{DeploymentID: 3, Gate: "e2e", Status: core.GatePassed},
			wantErr: gates.ErrDeploymentFinished,
		},
		{
			name:    "rollback without opt-in",
			req:     gates.ReportRequest{DeploymentID: 4, Gate: "e2e", Status: core.GatePassed},
			wantErr: gates.ErrGateNotApplicable,
		},
		{
			name:    "after gate before verifying",
			req:     gates.ReportRequest{DeploymentID: 2, Gate: "e2e", Status: core.GatePassed},
			wantErr: gates.ErrGateNotReady,
		},
		{
			name:    "after gate while queued",
			req:     gates.ReportRequest{DeploymentID: 5, Gate: "e2e", Status: core.GatePassed},
			wantErr: gates.ErrGateNotReady,
		},
		{
			name:    "skipped is not reportable",
			req:     gates.ReportRequest{DeploymentID: 1, Gate: "e2e", Status: core.GateSkipped},
			wantErr: core.ErrInvalidGateReport,
		},
		{
			name:    "bad url",
			req:     gates.ReportRequest{DeploymentID: 1, Gate: "e2e", Status: core.GatePassed, URL: "javascript:alert(1)"},
			wantErr: core.ErrInvalidGateReport,
		},
		{
			name:    "details not an object",
			req:     gates.ReportRequest{DeploymentID: 1, Gate: "e2e", Status: core.GatePassed, Details: json.RawMessage(`[]`)},
			wantErr: core.ErrInvalidGateReport,
		},
		{
			name:    "decided gate",
			req:     gates.ReportRequest{DeploymentID: 1, Gate: "e2e", Status: core.GateFailed},
			saveErr: core.ErrGateDecided,
			wantErr: core.ErrGateDecided,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := newReportStore()
			st.saveErr, st.unchanged = tt.saveErr, tt.unchanged
			reporter, err := gates.NewReporter(st, reportCatalog())
			if err != nil {
				t.Fatalf("NewReporter: %v", err)
			}
			req := tt.req
			req.Actor, req.Role = ciCaller, core.RoleReporter
			res, err := reporter.Report(t.Context(), req)

			if tt.wantLookup != "" && (len(st.lookups) != 1 || st.lookups[0] != tt.wantLookup) {
				t.Errorf("lookups = %v, want %s", st.lookups, tt.wantLookup)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Report = %v, want %v", err, tt.wantErr)
				}
				if tt.saveErr == nil && len(st.saved) != 0 {
					t.Errorf("rejected report was saved: %+v", st.saved)
				}
				return
			}
			if err != nil {
				t.Fatalf("Report: %v", err)
			}
			if res.Deployment.ID != tt.wantID || res.Phase != tt.wantPhase || res.Changed == tt.unchanged {
				t.Errorf("result = deployment %d phase %s changed %v", res.Deployment.ID, res.Phase, res.Changed)
			}
			if !tt.wantSaved || len(st.saved) != 1 {
				t.Fatalf("saved = %+v", st.saved)
			}
			saved := st.saved[0]
			if saved.DeploymentID != tt.wantID || saved.Gate != req.Gate || saved.Status != req.Status || saved.Message != req.Message || saved.ReportedBy != "token:ci" {
				t.Errorf("saved report = %+v", saved)
			}
			if res.Report.UpdatedAt != reportedAt {
				t.Errorf("result report = %+v, want the stored one", res.Report)
			}
			audit := st.audits[0]
			if audit.Action != core.AuditGateReport || audit.Actor != ciCaller || audit.Role != core.RoleReporter ||
				audit.Target == nil || *audit.Target != core.AppTarget("web") || audit.DeploymentID == nil || *audit.DeploymentID != tt.wantID {
				t.Errorf("audit = %+v", audit)
			}
			if err := audit.Validate(); err != nil {
				t.Errorf("audit invalid: %v", err)
			}
			var detail map[string]string
			if err := json.Unmarshal(audit.Detail, &detail); err != nil {
				t.Fatalf("audit detail %s: %v", audit.Detail, err)
			}
			if detail["gate"] != req.Gate || detail["phase"] != string(tt.wantPhase) || detail["status"] != string(req.Status) || detail["message"] != req.Message {
				t.Errorf("audit detail = %v", detail)
			}
		})
	}
}

func TestReporterCleansMessage(t *testing.T) {
	t.Parallel()

	st := newReportStore()
	reporter, err := gates.NewReporter(st, reportCatalog())
	if err != nil {
		t.Fatalf("NewReporter: %v", err)
	}
	_, err = reporter.Report(t.Context(), gates.ReportRequest{
		DeploymentID: 1, Gate: "e2e", Status: core.GatePassed, Message: "ok\x00", URL: "https://ci.example.com/1",
		Details: json.RawMessage(`{"a":1}`), Actor: ciCaller, Role: core.RoleReporter,
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	saved := st.saved[0]
	if saved.Message != "ok" || saved.URL != "https://ci.example.com/1" || string(saved.Details) != `{"a":1}` {
		t.Errorf("saved = %+v", saved)
	}
}
