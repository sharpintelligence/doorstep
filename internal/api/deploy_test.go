package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type fakeDeploys struct {
	mu        sync.Mutex
	deploys   []deploys.Request
	cancels   []deploys.CancelRequest
	created   bool
	deployErr error
	cancelErr error
}

func (f *fakeDeploys) Deploy(_ context.Context, req deploys.Request) (deploys.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploys = append(f.deploys, req)
	if f.deployErr != nil {
		return deploys.Result{}, f.deployErr
	}
	return deploys.Result{Deployment: sampleDeployment(12, core.KindDeploy, core.StateQueued, req.CommitSHA, ""), Created: f.created}, nil
}

func (f *fakeDeploys) Cancel(_ context.Context, req deploys.CancelRequest) (core.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, req)
	if f.cancelErr != nil {
		return core.Deployment{}, f.cancelErr
	}
	return sampleDeployment(req.ID, core.KindDeploy, core.StateCancelled, goodSHA, ""), nil
}

type tokenRoleAuth struct {
	role core.Role
}

func (a tokenRoleAuth) Authenticate(r *http.Request, required core.Role) (Caller, error) {
	if r.Header.Get("Authorization") != goodToken {
		return Caller{}, ErrUnauthenticated
	}
	if !a.role.Allows(required) {
		return Caller{}, ErrForbidden
	}
	return Caller{Actor: alice, Role: a.role}, nil
}

type nopHistory struct{}

func (nopHistory) ListDeployments(context.Context, store.DeploymentFilter) (store.DeploymentPage, error) {
	return store.DeploymentPage{}, nil
}

func (nopHistory) GetDeploymentHistory(context.Context, core.DeploymentID) (store.DeploymentHistory, error) {
	return store.DeploymentHistory{}, core.ErrNotFound
}

func TestDeployRoutesNeedEveryDependency(t *testing.T) {
	t.Parallel()

	tests := map[string]Options{
		"no deploy service": {KnownApp: knownApp, Authenticator: fakeAuth{}},
		"no app lookup":     {Deploys: &fakeDeploys{}, Authenticator: fakeAuth{}},
		"no authenticator":  {Deploys: &fakeDeploys{}, KnownApp: knownApp},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, path := range []string{"/api/v1/apps/web/deployments", "/api/v1/deployments/1/cancel"} {
				rec := do(t, New(opts), request{method: http.MethodPost, path: path, body: `{"commit_sha":"` + goodSHA + `"}`})
				if rec.Code != http.StatusNotFound {
					t.Errorf("POST %s status = %d, want 404 when the route is not registered", path, rec.Code)
				}
			}
		})
	}
}

func TestDeploymentsMethodNotAllowedListsPostWhenDeploysAreRegistered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
		want string
	}{
		{name: "history only", opts: Options{History: nopHistory{}}, want: "GET, HEAD"},
		{name: "history and deploys", opts: Options{History: nopHistory{}, Deploys: &fakeDeploys{}, KnownApp: knownApp, Authenticator: fakeAuth{}}, want: "GET, HEAD, POST"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, New(tt.opts), request{method: http.MethodPut, path: "/api/v1/apps/web/deployments"})
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != tt.want {
				t.Fatalf("PUT = %d Allow %q, want 405 Allow %q", rec.Code, rec.Header().Get("Allow"), tt.want)
			}
		})
	}
	rec := do(t, New(tests[1].opts), request{method: http.MethodGet, path: "/api/v1/deployments/1/cancel"})
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET cancel = %d Allow %q, want 405 Allow POST", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestDeployEndpoint(t *testing.T) {
	t.Parallel()

	block := sampleBlock()
	tests := []struct {
		name           string
		req            request
		role           core.Role
		created        bool
		deployErr      error
		wantStatus     int
		wantCode       string
		wantCalled     bool
		wantRetryAfter string
		check          func(t *testing.T, req deploys.Request, body map[string]any)
	}{
		{
			name:       "created",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `","urgent":true,"reason":"hotfix"}`},
			created:    true,
			wantStatus: http.StatusCreated,
			wantCalled: true,
			check: func(t *testing.T, req deploys.Request, body map[string]any) {
				t.Helper()
				want := deploys.Request{Target: web, CommitSHA: goodSHA, Urgent: true, Reason: "hotfix", Actor: alice, Role: core.RoleDeployer}
				if req != want {
					t.Errorf("request = %+v, want %+v", req, want)
				}
				if body["created"] != true || body["deployment"].(map[string]any)["id"] != float64(12) {
					t.Errorf("body = %v", body)
				}
			},
		},
		{
			name:       "existing deployment",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`},
			role:       core.RoleAdmin,
			wantStatus: http.StatusOK,
			wantCalled: true,
			check: func(t *testing.T, req deploys.Request, body map[string]any) {
				t.Helper()
				if req.Urgent || req.Role != core.RoleAdmin || body["created"] != false {
					t.Errorf("request = %+v, body = %v", req, body)
				}
			},
		},
		{name: "anonymous", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`, noAuth: true}, wantStatus: http.StatusUnauthorized, wantCode: "unauthorized"},
		{name: "viewer", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`}, role: core.RoleViewer, wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "reporter", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`}, role: core.RoleReporter, wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "unknown app", req: request{method: http.MethodPost, path: "/api/v1/apps/shop/deployments", body: `{"commit_sha":"` + goodSHA + `"}`}, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "missing body", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", contentType: "application/json"}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "not json", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: "commit_sha=x", contentType: "application/x-www-form-urlencoded"}, wantStatus: http.StatusUnsupportedMediaType, wantCode: "unsupported_media_type"},
		{name: "missing commit", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"urgent":true}`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "unknown field", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `","branch":"main"}`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "urgent must be a boolean", req: request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `","urgent":"yes"}`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{
			name:       "invalid sha",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"abc"}`},
			deployErr:  fmt.Errorf("%w: bad sha", deploys.ErrInvalidRequest),
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantCalled: true,
		},
		{
			name:       "commit not found",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`},
			deployErr:  fmt.Errorf("deploy: %w", deploys.ErrCommitNotFound),
			wantStatus: http.StatusNotFound, wantCode: "not_found", wantCalled: true,
		},
		{
			name:       "commit not on branch",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`},
			deployErr:  fmt.Errorf("deploy: %w", deploys.ErrCommitNotOnBranch),
			wantStatus: http.StatusConflict, wantCode: "commit_not_on_branch", wantCalled: true,
		},
		{
			name:       "blocked",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`},
			deployErr:  fmt.Errorf("deploy: %w", &core.BlockedError{Block: block}),
			wantStatus: http.StatusConflict, wantCode: "blocked", wantCalled: true,
			check: func(t *testing.T, _ deploys.Request, body map[string]any) {
				t.Helper()
				if b := body["block"].(map[string]any); b["id"] != float64(3) {
					t.Errorf("block = %v", b)
				}
			},
		},
		{
			name:       "github unavailable",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`},
			deployErr:  fmt.Errorf("%w: compare: %w", deploys.ErrUnavailable, errBoom),
			wantStatus: http.StatusServiceUnavailable, wantCode: "unavailable", wantCalled: true, wantRetryAfter: "5",
		},
		{
			name:       "internal error",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/deployments", body: `{"commit_sha":"` + goodSHA + `"}`},
			deployErr:  errBoom,
			wantStatus: http.StatusInternalServerError, wantCode: "internal", wantCalled: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			role := tt.role
			if role == "" {
				role = core.RoleDeployer
			}
			f := &fakeDeploys{created: tt.created, deployErr: tt.deployErr}
			var logs bytes.Buffer
			h := New(Options{Deploys: f, KnownApp: knownApp, Authenticator: tokenRoleAuth{role: role}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			rec := do(t, h, tt.req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("response leaks the error: %s", rec.Body.String())
			}
			if called := len(f.deploys) > 0; called != tt.wantCalled {
				t.Fatalf("Deploy called = %v, want %v", called, tt.wantCalled)
			}
			if tt.wantStatus < 300 {
				if loc := rec.Header().Get("Location"); loc != "/api/v1/deployments/12" {
					t.Errorf("Location = %q", loc)
				}
			}
			if tt.check != nil {
				var req deploys.Request
				if len(f.deploys) > 0 {
					req = f.deploys[0]
				}
				tt.check(t, req, decodeBody(t, rec.Body.String()))
			}
		})
	}
}

func TestCancelEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		req        request
		role       core.Role
		cancelErr  error
		wantStatus int
		wantCode   string
		wantReq    *deploys.CancelRequest
	}{
		{
			name:       "without a body",
			req:        request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel"},
			wantStatus: http.StatusOK,
			wantReq:    &deploys.CancelRequest{ID: 12, Actor: alice, Role: core.RoleDeployer},
		},
		{
			name:       "with a reason",
			req:        request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel", body: `{"reason":"wrong commit"}`},
			wantStatus: http.StatusOK,
			wantReq:    &deploys.CancelRequest{ID: 12, Reason: "wrong commit", Actor: alice, Role: core.RoleDeployer},
		},
		{name: "anonymous", req: request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel", noAuth: true}, wantStatus: http.StatusUnauthorized, wantCode: "unauthorized"},
		{name: "viewer", req: request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel"}, role: core.RoleViewer, wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "bad id", req: request{method: http.MethodPost, path: "/api/v1/deployments/012/cancel"}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "unknown field", req: request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel", body: `{"force":true}`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{
			name: "unknown deployment", req: request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel"},
			cancelErr: fmt.Errorf("cancel: %w", core.ErrNotFound), wantStatus: http.StatusNotFound, wantCode: "not_found",
			wantReq: &deploys.CancelRequest{ID: 12, Actor: alice, Role: core.RoleDeployer},
		},
		{
			name: "finished", req: request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel"},
			cancelErr: fmt.Errorf("cancel: %w", deploys.ErrNotCancellable), wantStatus: http.StatusConflict, wantCode: "not_cancellable",
			wantReq: &deploys.CancelRequest{ID: 12, Actor: alice, Role: core.RoleDeployer},
		},
		{
			name: "contended", req: request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel"},
			cancelErr: fmt.Errorf("cancel: %w", core.ErrStateConflict), wantStatus: http.StatusServiceUnavailable, wantCode: "contended",
			wantReq: &deploys.CancelRequest{ID: 12, Actor: alice, Role: core.RoleDeployer},
		},
		{
			name: "invalid reason", req: request{method: http.MethodPost, path: "/api/v1/deployments/12/cancel", body: `{"reason":"x"}`},
			cancelErr: fmt.Errorf("%w: too long", deploys.ErrInvalidRequest), wantStatus: http.StatusBadRequest, wantCode: "invalid_request",
			wantReq: &deploys.CancelRequest{ID: 12, Reason: "x", Actor: alice, Role: core.RoleDeployer},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			role := tt.role
			if role == "" {
				role = core.RoleDeployer
			}
			f := &fakeDeploys{cancelErr: tt.cancelErr}
			var logs bytes.Buffer
			h := New(Options{Deploys: f, KnownApp: knownApp, Authenticator: tokenRoleAuth{role: role}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			rec := do(t, h, tt.req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			}
			switch {
			case tt.wantReq == nil && len(f.cancels) != 0:
				t.Fatalf("Cancel called with %+v", f.cancels)
			case tt.wantReq != nil && (len(f.cancels) != 1 || f.cancels[0] != *tt.wantReq):
				t.Fatalf("Cancel requests = %+v, want %+v", f.cancels, *tt.wantReq)
			}
			if tt.wantStatus == http.StatusOK {
				body := decodeBody(t, rec.Body.String())
				if d := body["deployment"].(map[string]any); d["id"] != float64(12) || d["state"] != "cancelled" {
					t.Errorf("deployment = %v", d)
				}
			}
		})
	}
}
