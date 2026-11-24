package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
)

const (
	rollbackDigest core.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	badDigest      core.Digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	badSHA                     = "cccccccccccccccccccccccccccccccccccccccc"
	goodSHA                    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fakeRollbacks struct {
	mu          sync.Mutex
	requests    []rollback.Request
	rollbackErr error

	blockReqs []rollback.BlockRequest
	unblocks  []core.BlockRef
	blockErr  error
	created   bool
	listed    []core.Block
}

func (f *fakeRollbacks) Rollback(_ context.Context, req rollback.Request) (rollback.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.rollbackErr != nil {
		return rollback.Result{}, f.rollbackErr
	}
	bad := sampleDeployment(7, core.KindDeploy, core.StateCancelled, badSHA, badDigest)
	rb := sampleDeployment(9, core.KindRollback, core.StateQueued, goodSHA, rollbackDigest)
	rb.RollbackOfID = &bad.ID
	block := sampleBlock()
	return rollback.Result{Rollback: rb, RolledBack: bad, Block: &block, Cancelled: []core.DeploymentID{7, 8}}, nil
}

func (f *fakeRollbacks) Block(_ context.Context, req rollback.BlockRequest) (core.Block, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockReqs = append(f.blockReqs, req)
	if f.blockErr != nil {
		return core.Block{}, false, f.blockErr
	}
	b := sampleBlock()
	b.Digest, b.Commit, b.Reason, b.DeploymentID, b.RollbackID = req.Ref.Digest, core.Commit{SHA: req.Ref.CommitSHA}, req.Reason, nil, nil
	b.CreatedBy = req.Actor.String()
	return b, f.created, nil
}

func (f *fakeRollbacks) Unblock(_ context.Context, t core.Target, ref core.BlockRef, actor core.Actor, _ core.Role) ([]core.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unblocks = append(f.unblocks, ref)
	if f.blockErr != nil {
		return nil, f.blockErr
	}
	b := sampleBlock()
	b.Target = t
	at := createdAt
	b.ClearedAt, b.ClearedBy = &at, actor.String()
	return []core.Block{b}, nil
}

func (f *fakeRollbacks) ListBlocks(context.Context, core.Target) ([]core.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed, f.blockErr
}

func sampleDeployment(id core.DeploymentID, kind core.Kind, state core.State, sha string, digest core.Digest) core.Deployment {
	return core.Deployment{
		ID:          id,
		Target:      web,
		Kind:        kind,
		State:       state,
		Source:      core.SourceAPI,
		RequestedBy: "user:alice",
		Commit:      core.Commit{Repository: "acme/web", SHA: sha},
		Image:       core.ImageRef{Repository: "ghcr.io/acme/web", Tag: sha},
		Digest:      digest,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
}

func sampleBlock() core.Block {
	dep, rb := core.DeploymentID(7), core.DeploymentID(9)
	return core.Block{
		ID:             3,
		Target:         web,
		Digest:         badDigest,
		Commit:         core.Commit{Repository: "acme/web", SHA: badSHA},
		Reason:         "rolled back by user:alice",
		CreatedBy:      "user:alice",
		DeploymentID:   &dep,
		RollbackID:     &rb,
		CreatedAt:      createdAt,
		BlockCommitSHA: goodSHA,
	}
}

func newRollbackHandler(t *testing.T, f *fakeRollbacks, auth Authenticator, logs io.Writer) http.Handler {
	t.Helper()
	return New(Options{Rollbacks: f, Blocks: f, KnownApp: knownApp, Authenticator: auth, Logger: slog.New(slog.NewTextHandler(logs, nil))})
}

func decodeBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return got
}

func TestRollbackRoutesNeedEveryDependency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
	}{
		{"no rollback service", Options{Blocks: &fakeRollbacks{}, KnownApp: knownApp, Authenticator: fakeAuth{}}},
		{"no app lookup", Options{Rollbacks: &fakeRollbacks{}, Blocks: &fakeRollbacks{}, Authenticator: fakeAuth{}}},
		{"no authenticator", Options{Rollbacks: &fakeRollbacks{}, Blocks: &fakeRollbacks{}, KnownApp: knownApp}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, New(tt.opts), request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`})
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 when the route is not registered", rec.Code)
			}
		})
	}
}

func TestRollbackEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		req         request
		rollbackErr error
		wantStatus  int
		wantCode    string
		wantCalled  bool
		check       func(t *testing.T, req rollback.Request, body map[string]any)
	}{
		{
			name:       "one click with an empty body",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/rollback"},
			wantStatus: http.StatusCreated,
			wantCalled: true,
			check: func(t *testing.T, req rollback.Request, body map[string]any) {
				t.Helper()
				if req.Target != web || req.Actor != alice || req.Role != core.RoleDeployer || req.ToDeployment != nil || req.ToDigest != "" || req.Reason != "" {
					t.Errorf("request = %+v", req)
				}
				dep := body["deployment"].(map[string]any)
				if dep["id"] != float64(9) || dep["kind"] != "rollback" || dep["rollback_of_id"] != float64(7) {
					t.Errorf("deployment = %v", dep)
				}
				if rb := body["rolled_back"].(map[string]any); rb["id"] != float64(7) || rb["state"] != "cancelled" {
					t.Errorf("rolled_back = %v", rb)
				}
				block := body["block"].(map[string]any)
				if block["digest"] != string(badDigest) || block["recorded_in_git"] != true || block["rollback_id"] != float64(9) ||
					block["commit"].(map[string]any)["sha"] != badSHA {
					t.Errorf("block = %v", block)
				}
				if c := body["cancelled"].([]any); len(c) != 2 {
					t.Errorf("cancelled = %v", c)
				}
			},
		},
		{
			name:       "explicit deployment and reason",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{"to_deployment_id":4,"reason":"5xx"}`},
			wantStatus: http.StatusCreated,
			wantCalled: true,
			check: func(t *testing.T, req rollback.Request, _ map[string]any) {
				t.Helper()
				if req.ToDeployment == nil || *req.ToDeployment != 4 || req.Reason != "5xx" {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{
			name:       "explicit digest",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{"to_digest":"` + string(rollbackDigest) + `"}`},
			wantStatus: http.StatusCreated,
			wantCalled: true,
			check: func(t *testing.T, req rollback.Request, _ map[string]any) {
				t.Helper()
				if req.ToDigest != rollbackDigest {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{name: "unknown app", req: request{method: http.MethodPost, path: "/api/v1/apps/nope/rollback", body: `{}`}, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "both targets", req: request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{"to_deployment_id":4,"to_digest":"` + string(rollbackDigest) + `"}`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "bad digest", req: request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{"to_digest":"latest"}`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "unknown field", req: request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{"to":4}`}, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "wrong content type", req: request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`, contentType: "text/plain"}, wantStatus: http.StatusUnsupportedMediaType, wantCode: "unsupported_media_type"},
		{
			name:        "invalid rollback",
			req:         request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{"to_deployment_id":7}`},
			rollbackErr: fmt.Errorf("roll back: %w: deployment 7 is cancelled", core.ErrInvalidRollback),
			wantStatus:  http.StatusBadRequest, wantCode: "invalid_request", wantCalled: true,
		},
		{
			name:        "nothing to roll back to",
			req:         request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`},
			rollbackErr: fmt.Errorf("roll back: %w", core.ErrNoRollbackTarget),
			wantStatus:  http.StatusConflict, wantCode: "no_rollback_target", wantCalled: true,
		},
		{
			name: "rollback in progress",
			req:  request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`},
			rollbackErr: fmt.Errorf("roll back: %w", &core.RollbackInProgressError{
				Deployment: sampleDeployment(9, core.KindRollback, core.StateSyncing, goodSHA, rollbackDigest),
			}),
			wantStatus: http.StatusConflict, wantCode: "rollback_in_progress", wantCalled: true,
			check: func(t *testing.T, _ rollback.Request, body map[string]any) {
				t.Helper()
				if dep := body["deployment"].(map[string]any); dep["id"] != float64(9) || dep["state"] != "syncing" {
					t.Errorf("deployment = %v", dep)
				}
			},
		},
		{
			name:        "target is blocked",
			req:         request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{"to_deployment_id":7}`},
			rollbackErr: fmt.Errorf("roll back: %w", &core.BlockedError{Block: sampleBlock()}),
			wantStatus:  http.StatusConflict, wantCode: "blocked", wantCalled: true,
			check: func(t *testing.T, _ rollback.Request, body map[string]any) {
				t.Helper()
				if block := body["block"].(map[string]any); block["id"] != float64(3) {
					t.Errorf("block = %v", block)
				}
			},
		},
		{
			name:        "contended",
			req:         request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`},
			rollbackErr: fmt.Errorf("roll back: %w", core.ErrStateConflict),
			wantStatus:  http.StatusServiceUnavailable, wantCode: "contended", wantCalled: true,
		},
		{
			name:        "internal error",
			req:         request{method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`},
			rollbackErr: errBoom,
			wantStatus:  http.StatusInternalServerError, wantCode: "internal", wantCalled: true,
		},
		{name: "wrong method", req: request{method: http.MethodGet, path: "/api/v1/apps/web/rollback"}, wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRollbacks{rollbackErr: tt.rollbackErr}
			var logs strings.Builder
			rec := do(t, newRollbackHandler(t, f, fakeAuth{}, &logs), tt.req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("error code = %q, want %q", code, tt.wantCode)
				}
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("response leaks the internal error: %s", rec.Body)
			}
			if (len(f.requests) == 1) != tt.wantCalled {
				t.Fatalf("service called %d times, want called %v", len(f.requests), tt.wantCalled)
			}
			if tt.wantStatus == http.StatusCreated {
				if loc := rec.Header().Get("Location"); loc != "/api/v1/deployments/9" {
					t.Errorf("Location = %q", loc)
				}
			}
			if tt.wantStatus == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
			if tt.check != nil {
				var req rollback.Request
				if len(f.requests) > 0 {
					req = f.requests[0]
				}
				tt.check(t, req, decodeBody(t, rec.Body.String()))
			}
		})
	}
}

func TestRollbackAndBlockAuthorization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       core.Role
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{name: "viewer cannot roll back", role: core.RoleViewer, method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`, wantStatus: http.StatusForbidden},
		{name: "deployer rolls back", role: core.RoleDeployer, method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`, wantStatus: http.StatusCreated},
		{name: "admin rolls back", role: core.RoleAdmin, method: http.MethodPost, path: "/api/v1/apps/web/rollback", body: `{}`, wantStatus: http.StatusCreated},
		{name: "viewer lists blocks", role: core.RoleViewer, method: http.MethodGet, path: "/api/v1/apps/web/blocks", wantStatus: http.StatusOK},
		{name: "viewer cannot block", role: core.RoleViewer, method: http.MethodPost, path: "/api/v1/apps/web/blocks", body: `{"digest":"` + string(badDigest) + `","reason":"r"}`, wantStatus: http.StatusForbidden},
		{name: "viewer cannot unblock", role: core.RoleViewer, method: http.MethodDelete, path: "/api/v1/apps/web/blocks/" + string(badDigest), wantStatus: http.StatusForbidden},
		{name: "deployer unblocks", role: core.RoleDeployer, method: http.MethodDelete, path: "/api/v1/apps/web/blocks/" + string(badDigest), wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRollbacks{}
			rec := do(t, newRollbackHandler(t, f, roleAuth{role: tt.role}, io.Discard), request{method: tt.method, path: tt.path, body: tt.body})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantStatus == http.StatusForbidden && (len(f.requests)+len(f.blockReqs)+len(f.unblocks)) != 0 {
				t.Error("forbidden request reached the service")
			}
		})
	}

	for _, path := range []string{"/api/v1/apps/web/rollback", "/api/v1/apps/web/blocks", "/api/v1/apps/web/blocks/" + badSHA} {
		rec := do(t, newRollbackHandler(t, &fakeRollbacks{}, fakeAuth{}, io.Discard), request{method: http.MethodPost, path: path, body: `{}`, noAuth: true})
		if path == "/api/v1/apps/web/blocks/"+badSHA {
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want 405", path, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("unauthenticated POST %s = %d", path, rec.Code)
		}
	}
}
