package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

const gateSHA = "0123456789abcdef0123456789abcdef01234567"

type fakeGateReporter struct {
	mu   sync.Mutex
	reqs []gates.ReportRequest
	err  error
}

func (f *fakeGateReporter) Report(_ context.Context, req gates.ReportRequest) (gates.ReportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return gates.ReportResult{}, f.err
	}
	id := req.DeploymentID
	if id == 0 {
		id = 12
	}
	return gates.ReportResult{
		Deployment: core.Deployment{ID: id, Target: web},
		Phase:      core.GatePhaseAfter,
		Report: core.GateReport{
			DeploymentID: id,
			Gate:         req.Gate,
			Status:       req.Status,
			Message:      req.Message,
			URL:          req.URL,
			Details:      req.Details,
			ReportedBy:   req.Actor.String(),
			CreatedAt:    createdAt,
			UpdatedAt:    expiresAt,
		},
		Changed: true,
	}, nil
}

func (f *fakeGateReporter) requests() []gates.ReportRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gates.ReportRequest(nil), f.reqs...)
}

func newGateHandler(reports GateReporter, auth Authenticator, logs io.Writer) http.Handler {
	return New(Options{GateReports: reports, KnownApp: knownApp, Authenticator: auth, Logger: slog.New(slog.NewTextHandler(logs, nil))})
}

func TestGateReportRoutesNeedEveryDependency(t *testing.T) {
	t.Parallel()

	for name, opts := range map[string]Options{
		"no reporter":      {KnownApp: knownApp, Authenticator: fakeAuth{}},
		"no known apps":    {GateReports: &fakeGateReporter{}, Authenticator: fakeAuth{}},
		"no authenticator": {GateReports: &fakeGateReporter{}, KnownApp: knownApp},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, New(opts), request{method: http.MethodPost, path: "/api/v1/deployments/1/gates/e2e", body: `{"status":"passed"}`})
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestReportGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		body       string
		wantReq    gates.ReportRequest
		wantBody   string
		wantStatus int
	}{
		{
			name: "by deployment",
			path: "/api/v1/deployments/7/gates/e2e",
			body: `{"status":"passed","message":"12 tests passed","url":"https://ci.example.com/runs/1","details":{"tests":12}}`,
			wantReq: gates.ReportRequest{
				DeploymentID: 7, Gate: "e2e", Status: core.GatePassed, Message: "12 tests passed",
				URL: "https://ci.example.com/runs/1", Details: json.RawMessage(`{"tests":12}`),
			},
			wantStatus: http.StatusOK,
			wantBody: `{"deployment_id":7,"app":"web","gate":"e2e","phase":"after","status":"passed","message":"12 tests passed",` +
				`"url":"https://ci.example.com/runs/1","details":{"tests":12},"reported_by":"user:alice",` +
				`"reported_at":"2026-11-23T12:00:00Z","updated_at":"2026-11-23T14:00:00Z"}`,
		},
		{
			name:       "by commit",
			path:       "/api/v1/apps/web/commits/" + gateSHA + "/gates/e2e",
			body:       `{"status":"pending"}`,
			wantReq:    gates.ReportRequest{App: "web", CommitSHA: gateSHA, Gate: "e2e", Status: core.GatePending},
			wantStatus: http.StatusOK,
			wantBody: `{"deployment_id":12,"app":"web","gate":"e2e","phase":"after","status":"pending","message":"","url":"",` +
				`"details":{},"reported_by":"user:alice","reported_at":"2026-11-23T12:00:00Z","updated_at":"2026-11-23T14:00:00Z"}`,
		},
		{
			name:       "null details",
			path:       "/api/v1/deployments/7/gates/e2e",
			body:       `{"status":"failed","details":null}`,
			wantReq:    gates.ReportRequest{DeploymentID: 7, Gate: "e2e", Status: core.GateFailed},
			wantStatus: http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reports := &fakeGateReporter{}
			rec := do(t, newGateHandler(reports, fakeAuth{}, io.Discard), request{method: http.MethodPost, path: tt.path, body: tt.body})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			reqs := reports.requests()
			if len(reqs) != 1 {
				t.Fatalf("requests = %+v", reqs)
			}
			got := reqs[0]
			want := tt.wantReq
			want.Actor, want.Role = alice, core.RoleReporter
			if got.DeploymentID != want.DeploymentID || got.App != want.App || got.CommitSHA != want.CommitSHA || got.Gate != want.Gate ||
				got.Status != want.Status || got.Message != want.Message || got.URL != want.URL || string(got.Details) != string(want.Details) ||
				got.Actor != want.Actor || got.Role != want.Role {
				t.Errorf("request = %+v\nwant %+v", got, want)
			}
			if tt.wantBody != "" && strings.TrimSpace(rec.Body.String()) != tt.wantBody {
				t.Errorf("body = %s\nwant %s", rec.Body, tt.wantBody)
			}
		})
	}
}

func TestReportGateErrors(t *testing.T) {
	t.Parallel()

	byID := "/api/v1/deployments/7/gates/e2e"
	byCommit := "/api/v1/apps/web/commits/" + gateSHA + "/gates/e2e"
	tests := []struct {
		name           string
		path           string
		body           string
		contentType    string
		err            error
		wantStatus     int
		wantCode       string
		wantMessage    string
		wantRetryAfter string
		wantCalled     bool
	}{
		{name: "zero id", path: "/api/v1/deployments/0/gates/e2e", body: `{"status":"passed"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "non-numeric id", path: "/api/v1/deployments/abc/gates/e2e", body: `{"status":"passed"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "unknown app", path: "/api/v1/apps/nope/commits/" + gateSHA + "/gates/e2e", body: `{"status":"passed"}`, wantStatus: http.StatusNotFound, wantCode: "not_found", wantMessage: "unknown app nope"},
		{name: "short sha", path: "/api/v1/apps/web/commits/abc/gates/e2e", body: `{"status":"passed"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "uppercase sha", path: "/api/v1/apps/web/commits/" + strings.ToUpper(gateSHA) + "/gates/e2e", body: `{"status":"passed"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "missing status", path: byID, body: `{"message":"hi"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantMessage: "status is required (want pending, passed or failed)"},
		{name: "unknown field", path: byID, body: `{"status":"passed","state":"success"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "wrong type", path: byID, body: `{"status":"passed","details":"x","message":3}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "two objects", path: byID, body: `{"status":"passed"}{}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "empty body", path: byID, wantStatus: http.StatusUnsupportedMediaType, wantCode: "unsupported_media_type"},
		{name: "not json", path: byID, body: `status=passed`, contentType: "application/x-www-form-urlencoded", wantStatus: http.StatusUnsupportedMediaType, wantCode: "unsupported_media_type"},
		{name: "oversized body", path: byID, body: `{"status":"passed","message":"` + strings.Repeat("x", MaxRequestBytes) + `"}`, wantStatus: http.StatusRequestEntityTooLarge, wantCode: "invalid_request"},
		{name: "deployment not found", path: byID, body: `{"status":"passed"}`, err: fmt.Errorf("deployment 7: %w", core.ErrNotFound), wantStatus: http.StatusNotFound, wantCode: "not_found", wantMessage: "deployment 7 not found", wantCalled: true},
		{name: "no deployment for commit", path: byCommit, body: `{"status":"passed"}`, err: core.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found", wantMessage: "no unfinished deployment of web for commit " + gateSHA, wantCalled: true},
		{name: "unknown gate", path: byID, body: `{"status":"passed"}`, err: fmt.Errorf("%w e2e", gates.ErrUnknownGate), wantStatus: http.StatusNotFound, wantCode: "not_found", wantMessage: "unknown gate e2e", wantCalled: true},
		{name: "invalid report", path: byID, body: `{"status":"skipped"}`, err: fmt.Errorf("%w: status skipped cannot be reported", core.ErrInvalidGateReport), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantCalled: true},
		{name: "reporter not allowed", path: byID, body: `{"status":"passed"}`, err: fmt.Errorf("%w: token:other", gates.ErrReporterNotAllowed), wantStatus: http.StatusForbidden, wantCode: "forbidden", wantMessage: "gate e2e accepts results only from its configured reporters", wantCalled: true},
		{name: "built-in gate", path: byID, body: `{"status":"passed"}`, err: gates.ErrGateNotExternal, wantStatus: http.StatusConflict, wantCode: "gate_not_external", wantCalled: true},
		{name: "finished", path: byID, body: `{"status":"passed"}`, err: gates.ErrDeploymentFinished, wantStatus: http.StatusConflict, wantCode: "deployment_finished", wantCalled: true},
		{name: "rollback", path: byID, body: `{"status":"passed"}`, err: gates.ErrGateNotApplicable, wantStatus: http.StatusConflict, wantCode: "gate_not_applicable", wantCalled: true},
		{name: "not ready", path: byID, body: `{"status":"passed"}`, err: gates.ErrGateNotReady, wantStatus: http.StatusConflict, wantCode: "gate_not_ready", wantRetryAfter: "5", wantCalled: true},
		{name: "decided", path: byID, body: `{"status":"failed"}`, err: fmt.Errorf("save: %w", core.ErrGateDecided), wantStatus: http.StatusConflict, wantCode: "gate_decided", wantMessage: "gate e2e already has a final result; only the same verdict can be reported again", wantCalled: true},
		{name: "internal", path: byID, body: `{"status":"passed"}`, err: errBoom, wantStatus: http.StatusInternalServerError, wantCode: "internal", wantMessage: "internal error", wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reports := &fakeGateReporter{err: tt.err}
			var logs strings.Builder
			var mu sync.Mutex
			h := newGateHandler(reports, fakeAuth{}, writerFunc(func(p []byte) (int, error) {
				mu.Lock()
				defer mu.Unlock()
				return logs.Write(p)
			}))
			rec := do(t, h, request{method: http.MethodPost, path: tt.path, body: tt.body, contentType: tt.contentType})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			var body errorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s: %v", rec.Body, err)
			}
			if body.Error.Code != tt.wantCode || body.Error.Message == "" {
				t.Errorf("error = %+v, want code %s", body.Error, tt.wantCode)
			}
			if tt.wantMessage != "" && body.Error.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", body.Error.Message, tt.wantMessage)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			if called := len(reports.requests()) > 0; called != tt.wantCalled {
				t.Errorf("reporter called = %v, want %v", called, tt.wantCalled)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("internal error leaked: %s", rec.Body)
			}
			if tt.wantStatus == http.StatusInternalServerError && !strings.Contains(logs.String(), "hunter2") {
				t.Errorf("internal error not logged: %s", logs.String())
			}
		})
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestReportGateRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       core.Role
		noAuth     bool
		wantStatus int
	}{
		{name: "anonymous", noAuth: true, wantStatus: http.StatusUnauthorized},
		{name: "viewer", role: core.RoleViewer, wantStatus: http.StatusForbidden},
		{name: "reporter", role: core.RoleReporter, wantStatus: http.StatusOK},
		{name: "deployer", role: core.RoleDeployer, wantStatus: http.StatusOK},
		{name: "admin", role: core.RoleAdmin, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		for _, path := range []string{"/api/v1/deployments/7/gates/e2e", "/api/v1/apps/web/commits/" + gateSHA + "/gates/e2e"} {
			t.Run(tt.name+" "+path, func(t *testing.T) {
				t.Parallel()
				reports := &fakeGateReporter{}
				var auth Authenticator = roleAuth{role: tt.role}
				if tt.noAuth {
					auth = fakeAuth{}
				}
				rec := do(t, newGateHandler(reports, auth, io.Discard), request{method: http.MethodPost, path: path, body: `{"status":"passed"}`, noAuth: tt.noAuth})
				if rec.Code != tt.wantStatus {
					t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
				}
				reqs := reports.requests()
				if tt.wantStatus != http.StatusOK {
					if len(reqs) != 0 {
						t.Errorf("rejected request reached the reporter: %+v", reqs)
					}
					return
				}
				if len(reqs) != 1 || reqs[0].Role != tt.role || reqs[0].Actor != alice {
					t.Errorf("requests = %+v, want the caller's role %s", reqs, tt.role)
				}
			})
		}
	}
}

func TestReportGateMethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newGateHandler(&fakeGateReporter{}, fakeAuth{}, io.Discard)
	for _, path := range []string{"/api/v1/deployments/7/gates/e2e", "/api/v1/apps/web/commits/" + gateSHA + "/gates/e2e"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			r := httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
				t.Errorf("%s %s = %d (Allow %q), want 405 with Allow POST", method, path, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
}
