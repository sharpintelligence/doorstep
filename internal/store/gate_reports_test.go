package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

var ciActor = core.Actor{Kind: core.ActorToken, ID: "ci"}

func gateReport(id core.DeploymentID, status core.GateStatus) core.GateReport {
	return core.GateReport{
		DeploymentID: id,
		Gate:         "e2e",
		Status:       status,
		Message:      "e2e " + string(status),
		URL:          "https://ci.example.com/runs/1",
		Details:      json.RawMessage(`{"suite": "smoke"}`),
		ReportedBy:   ciActor.String(),
	}
}

func reportAudit(d core.Deployment, status core.GateStatus) core.AuditEntry {
	return core.AuditEntry{
		Actor:        ciActor,
		Role:         core.RoleReporter,
		Action:       core.AuditGateReport,
		Target:       &d.Target,
		DeploymentID: &d.ID,
		Detail:       json.RawMessage(`{"gate":"e2e","status":"` + string(status) + `"}`),
	}
}

func mustSaveReport(t *testing.T, st *store.Store, d core.Deployment, r core.GateReport) (core.GateReport, bool) {
	t.Helper()
	stored, changed, err := st.SaveGateReport(t.Context(), r, reportAudit(d, r.Status))
	if err != nil {
		t.Fatalf("SaveGateReport(%s): %v", r.Status, err)
	}
	return stored, changed
}

func countReportAudits(t *testing.T, st *store.Store, id core.DeploymentID) int {
	t.Helper()
	return queryInt(t, st, "SELECT count(*) FROM audit_log WHERE action = 'gate.report' AND deployment_id = $1", id)
}

func TestSaveGateReportInserts(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)

	stored, changed := mustSaveReport(t, st, d, gateReport(d.ID, core.GatePending))
	if !changed {
		t.Error("first report reported unchanged")
	}
	if stored.DeploymentID != d.ID || stored.Gate != "e2e" || stored.Status != core.GatePending ||
		stored.Message != "e2e pending" || stored.URL != "https://ci.example.com/runs/1" || stored.ReportedBy != "token:ci" {
		t.Errorf("stored = %+v", stored)
	}
	if string(stored.Details) != `{"suite": "smoke"}` {
		t.Errorf("details = %s", stored.Details)
	}
	if stored.CreatedAt.IsZero() || stored.UpdatedAt.Before(stored.CreatedAt) {
		t.Errorf("CreatedAt = %v, UpdatedAt = %v", stored.CreatedAt, stored.UpdatedAt)
	}
	got, err := st.GetGateReport(t.Context(), d.ID, "e2e")
	if err != nil {
		t.Fatalf("GetGateReport: %v", err)
	}
	if got.Status != core.GatePending || got.Message != stored.Message {
		t.Errorf("GetGateReport = %+v", got)
	}
	if n := countReportAudits(t, st, d.ID); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
	entries, err := st.ListAudit(t.Context(), store.AuditFilter{Action: core.AuditGateReport})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 || entries[0].Role != core.RoleReporter || entries[0].Actor != ciActor || entries[0].Target == nil || *entries[0].Target != d.Target {
		t.Errorf("audit = %+v", entries)
	}
}

func TestSaveGateReportDefaultsDetails(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	r := gateReport(d.ID, core.GatePassed)
	r.Details, r.URL, r.Message = nil, "", "bad\x00message"

	stored, _ := mustSaveReport(t, st, d, r)
	if string(stored.Details) != "{}" || stored.URL != "" || stored.Message != "badmessage" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestSaveGateReportTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		first       func(core.DeploymentID) core.GateReport
		second      func(core.DeploymentID) core.GateReport
		wantChanged bool
		wantErr     error
		wantStatus  core.GateStatus
		wantMessage string
		wantAudits  int
	}{
		{
			name:        "pending to passed",
			first:       func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePending) },
			second:      func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePassed) },
			wantChanged: true, wantStatus: core.GatePassed, wantMessage: "e2e passed", wantAudits: 2,
		},
		{
			name:  "pending progress update",
			first: func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePending) },
			second: func(id core.DeploymentID) core.GateReport {
				r := gateReport(id, core.GatePending)
				r.Message = "3 of 10 suites done"
				return r
			},
			wantChanged: true, wantStatus: core.GatePending, wantMessage: "3 of 10 suites done", wantAudits: 2,
		},
		{
			name:  "pending details change",
			first: func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePending) },
			second: func(id core.DeploymentID) core.GateReport {
				r := gateReport(id, core.GatePending)
				r.Details = json.RawMessage(`{"suite":"full"}`)
				return r
			},
			wantChanged: true, wantStatus: core.GatePending, wantMessage: "e2e pending", wantAudits: 2,
		},
		{
			name:       "identical pending retry",
			first:      func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePending) },
			second:     func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePending) },
			wantStatus: core.GatePending, wantMessage: "e2e pending", wantAudits: 1,
		},
		{
			name:  "equivalent details retry",
			first: func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePending) },
			second: func(id core.DeploymentID) core.GateReport {
				r := gateReport(id, core.GatePending)
				r.Details = json.RawMessage(`{"suite":"smoke"}`)
				return r
			},
			wantStatus: core.GatePending, wantMessage: "e2e pending", wantAudits: 1,
		},
		{
			name:       "identical terminal retry",
			first:      func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePassed) },
			second:     func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePassed) },
			wantStatus: core.GatePassed, wantMessage: "e2e passed", wantAudits: 1,
		},
		{
			name:  "same verdict with another message keeps the first",
			first: func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GateFailed) },
			second: func(id core.DeploymentID) core.GateReport {
				r := gateReport(id, core.GateFailed)
				r.Message = "retry"
				return r
			},
			wantStatus: core.GateFailed, wantMessage: "e2e failed", wantAudits: 1,
		},
		{
			name:       "passed then failed",
			first:      func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePassed) },
			second:     func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GateFailed) },
			wantErr:    core.ErrGateDecided,
			wantStatus: core.GatePassed, wantMessage: "e2e passed", wantAudits: 1,
		},
		{
			name:       "failed then pending",
			first:      func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GateFailed) },
			second:     func(id core.DeploymentID) core.GateReport { return gateReport(id, core.GatePending) },
			wantErr:    core.ErrGateDecided,
			wantStatus: core.GateFailed, wantMessage: "e2e failed", wantAudits: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			mustSaveReport(t, st, d, tt.first(d.ID))

			second := tt.second(d.ID)
			stored, changed, err := st.SaveGateReport(t.Context(), second, reportAudit(d, second.Status))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("SaveGateReport = %v, want %v", err, tt.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("SaveGateReport: %v", err)
				}
				if changed != tt.wantChanged {
					t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
				}
				if stored.Status != tt.wantStatus || stored.Message != tt.wantMessage {
					t.Errorf("returned %s %q, want %s %q", stored.Status, stored.Message, tt.wantStatus, tt.wantMessage)
				}
			}
			got, err := st.GetGateReport(t.Context(), d.ID, "e2e")
			if err != nil {
				t.Fatalf("GetGateReport: %v", err)
			}
			if got.Status != tt.wantStatus || got.Message != tt.wantMessage {
				t.Errorf("stored %s %q, want %s %q", got.Status, got.Message, tt.wantStatus, tt.wantMessage)
			}
			if n := countReportAudits(t, st, d.ID); n != tt.wantAudits {
				t.Errorf("audit rows = %d, want %d", n, tt.wantAudits)
			}
		})
	}
}

func TestSaveGateReportRejectsDecidedGateResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		result     core.GateStatus
		preReport  bool
		report     core.GateStatus
		wantErr    error
		wantReport bool
	}{
		{name: "timed out before any report", result: core.GateFailed, report: core.GatePassed, wantErr: core.ErrGateDecided},
		{name: "timed out after a pending report", result: core.GateFailed, preReport: true, report: core.GatePassed, wantErr: core.ErrGateDecided, wantReport: true},
		{name: "skipped gate", result: core.GateSkipped, report: core.GatePending, wantErr: core.ErrGateDecided},
		{name: "pending gate result accepts", result: core.GatePending, report: core.GatePassed, wantReport: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			if tt.preReport {
				mustSaveReport(t, st, d, gateReport(d.ID, core.GatePending))
			}
			result := gateResult(d.ID, "e2e", tt.result)
			result.Type = "external"
			mustRecord(t, st, result)

			_, _, err := st.SaveGateReport(t.Context(), gateReport(d.ID, tt.report), reportAudit(d, tt.report))
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("SaveGateReport = %v, want %v", err, tt.wantErr)
			}
			_, getErr := st.GetGateReport(t.Context(), d.ID, "e2e")
			if found := getErr == nil; found != tt.wantReport {
				t.Errorf("report found = %v (%v), want %v", found, getErr, tt.wantReport)
			}
			wantAudits := 0
			if tt.preReport {
				wantAudits++
			}
			if tt.wantErr == nil {
				wantAudits++
			}
			if n := countReportAudits(t, st, d.ID); n != wantAudits {
				t.Errorf("audit rows = %d, want %d", n, wantAudits)
			}
		})
	}
}

func externalResult(id core.DeploymentID, status core.GateStatus) core.GateResult {
	r := gateResult(id, "e2e", status)
	r.Type = "external"
	return r
}

func TestRecordGateResultRespectsReportedVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		report  core.GateStatus
		record  core.GateStatus
		wantErr error
	}{
		{name: "no report, timeout", record: core.GateFailed},
		{name: "pending report, timeout", report: core.GatePending, record: core.GateFailed},
		{name: "passed report, timeout", report: core.GatePassed, record: core.GateFailed, wantErr: core.ErrGateReported},
		{name: "failed report, passed result", report: core.GateFailed, record: core.GatePassed, wantErr: core.ErrGateReported},
		{name: "passed report, passed result", report: core.GatePassed, record: core.GatePassed},
		{name: "passed report, pending result", report: core.GatePassed, record: core.GatePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			mustRecord(t, st, externalResult(d.ID, core.GatePending))
			if tt.report != "" {
				mustSaveReport(t, st, d, gateReport(d.ID, tt.report))
			}

			_, _, err := st.RecordGateResult(t.Context(), externalResult(d.ID, tt.record))
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("RecordGateResult = %v, want %v", err, tt.wantErr)
			}
			results, err := st.ListGateResults(t.Context(), d.ID)
			if err != nil {
				t.Fatalf("ListGateResults: %v", err)
			}
			want := tt.record
			if tt.wantErr != nil {
				want = core.GatePending
			}
			if len(results) != 1 || results[0].Status != want {
				t.Errorf("results = %+v, want one %s", results, want)
			}
		})
	}
}

func TestReportAndTimeoutRaceHasOneWinner(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	const rounds = 20
	for range rounds {
		d := mustCreate(t, st, newDeployment("web"), 0)
		mustRecord(t, st, externalResult(d.ID, core.GatePending))

		var (
			wg        sync.WaitGroup
			reportErr error
			recordErr error
		)
		wg.Go(func() {
			_, _, reportErr = st.SaveGateReport(t.Context(), gateReport(d.ID, core.GatePassed), reportAudit(d, core.GatePassed))
		})
		wg.Go(func() {
			_, _, recordErr = st.RecordGateResult(t.Context(), externalResult(d.ID, core.GateFailed))
		})
		wg.Wait()

		switch {
		case reportErr == nil && errors.Is(recordErr, core.ErrGateReported):
		case recordErr == nil && errors.Is(reportErr, core.ErrGateDecided):
		default:
			t.Fatalf("deployment %d: report = %v, timeout = %v; want exactly one to win", d.ID, reportErr, recordErr)
		}
	}
}

func TestSaveGateReportTerminalRetryAfterGateDecided(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	mustSaveReport(t, st, d, gateReport(d.ID, core.GatePassed))
	result := gateResult(d.ID, "e2e", core.GatePassed)
	result.Type = "external"
	mustRecord(t, st, result)

	stored, changed := mustSaveReport(t, st, d, gateReport(d.ID, core.GatePassed))
	if changed || stored.Status != core.GatePassed {
		t.Errorf("retry = %s changed=%v, want an unchanged passed report", stored.Status, changed)
	}
	if _, _, err := st.SaveGateReport(t.Context(), gateReport(d.ID, core.GateFailed), reportAudit(d, core.GateFailed)); !errors.Is(err, core.ErrGateDecided) {
		t.Errorf("flip = %v, want ErrGateDecided", err)
	}
}

func TestSaveGateReportErrors(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)

	missing := gateReport(d.ID+1000, core.GatePassed)
	if _, _, err := st.SaveGateReport(t.Context(), missing, reportAudit(d, core.GatePassed)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown deployment = %v, want ErrNotFound", err)
	}
	invalid := gateReport(d.ID, core.GateSkipped)
	if _, _, err := st.SaveGateReport(t.Context(), invalid, reportAudit(d, core.GatePassed)); !errors.Is(err, core.ErrInvalidGateReport) {
		t.Errorf("skipped = %v, want ErrInvalidGateReport", err)
	}
	badAudit := reportAudit(d, core.GatePassed)
	badAudit.Actor = core.Actor{}
	if _, _, err := st.SaveGateReport(t.Context(), gateReport(d.ID, core.GatePassed), badAudit); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Errorf("invalid audit = %v, want an audit error", err)
	}
	if n := queryInt(t, st, "SELECT count(*) FROM gate_reports"); n != 0 {
		t.Errorf("gate_reports rows = %d after failures, want 0", n)
	}
	if _, err := st.GetGateReport(t.Context(), d.ID, "e2e"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("GetGateReport = %v, want ErrNotFound", err)
	}
}

func TestSaveGateReportConcurrentFirstReports(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	changed := make([]bool, n)
	for i := range n {
		wg.Go(func() {
			_, changed[i], errs[i] = st.SaveGateReport(t.Context(), gateReport(d.ID, core.GatePassed), reportAudit(d, core.GatePassed))
		})
	}
	wg.Wait()
	writes := 0
	for i := range n {
		if errs[i] != nil {
			t.Errorf("report %d: %v", i, errs[i])
		}
		if changed[i] {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("%d reports changed the row, want exactly 1", writes)
	}
	if got := countReportAudits(t, st, d.ID); got != 1 {
		t.Errorf("audit rows = %d, want 1", got)
	}
}

func TestGateReportsCascadeWithDeployment(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	mustSaveReport(t, st, d, gateReport(d.ID, core.GatePassed))

	mustExec(t, st, "DELETE FROM queue_entries WHERE deployment_id = $1", d.ID)
	mustExec(t, st, "DELETE FROM deployments WHERE id = $1", d.ID)
	if n := queryInt(t, st, "SELECT count(*) FROM gate_reports WHERE deployment_id = $1", d.ID); n != 0 {
		t.Errorf("gate_reports rows = %d after deleting the deployment, want 0", n)
	}
}

func TestSchemaRejectsInvalidGateReports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sql        string
		constraint string
	}{
		{name: "blank gate", sql: "INSERT INTO gate_reports (deployment_id, gate, status, reported_by) VALUES ($1, ' ', 'passed', 'token:ci')", constraint: "gate_reports_gate_check"},
		{name: "skipped status", sql: "INSERT INTO gate_reports (deployment_id, gate, status, reported_by) VALUES ($1, 'e2e', 'skipped', 'token:ci')", constraint: "gate_reports_status_check"},
		{name: "array details", sql: "INSERT INTO gate_reports (deployment_id, gate, status, details, reported_by) VALUES ($1, 'e2e', 'passed', '[]', 'token:ci')", constraint: "gate_reports_details_check"},
		{name: "blank reporter", sql: "INSERT INTO gate_reports (deployment_id, gate, status, reported_by) VALUES ($1, 'e2e', 'passed', '')", constraint: "gate_reports_reported_by_check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			assertConstraint(t, store.ExecSQL(t.Context(), st, tt.sql, d.ID), tt.constraint)
		})
	}
}

func TestActiveDeploymentForCommit(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	target := core.Target{App: "web", Environment: "prod"}

	if _, err := st.ActiveDeploymentForCommit(t.Context(), target, testSHA); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("no deployments = %v, want ErrNotFound", err)
	}

	mustCreateIn(t, st, deploymentOf("web", testSHA), core.StateSucceeded)
	if _, err := st.ActiveDeploymentForCommit(t.Context(), target, testSHA); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("only a finished deployment = %v, want ErrNotFound", err)
	}

	older := mustCreate(t, st, deploymentOf("web", testSHA), 0)
	newer := mustCreate(t, st, deploymentOf("web", testSHA), 0)
	mustCreate(t, st, deploymentOf("web", otherSHA), 0)
	mustCreate(t, st, deploymentOf("api", testSHA), 0)

	got, err := st.ActiveDeploymentForCommit(t.Context(), target, testSHA)
	if err != nil {
		t.Fatalf("ActiveDeploymentForCommit: %v", err)
	}
	if got.ID != newer.ID || got.Commit.SHA != testSHA || got.Target != target {
		t.Errorf("got deployment %d (%s %s), want %d", got.ID, got.Target, got.Commit.SHA, newer.ID)
	}

	mustExec(t, st, "DELETE FROM queue_entries WHERE deployment_id = $1", newer.ID)
	mustExec(t, st, "UPDATE deployments SET state = 'cancelled', finished_at = now() WHERE id = $1", newer.ID)
	got, err = st.ActiveDeploymentForCommit(t.Context(), target, testSHA)
	if err != nil || got.ID != older.ID {
		t.Errorf("after cancelling the newer one = %d, %v, want %d", got.ID, err, older.ID)
	}

	if _, err := st.ActiveDeploymentForCommit(t.Context(), target, "abc"); !errors.Is(err, core.ErrInvalidCommitSHA) {
		t.Errorf("bad sha = %v, want ErrInvalidCommitSHA", err)
	}
	if _, err := st.ActiveDeploymentForCommit(t.Context(), core.Target{App: "web"}, testSHA); err == nil {
		t.Error("target without environment accepted")
	}
}
