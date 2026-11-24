package api

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestListBlocksEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		listed     []core.Block
		listErr    error
		wantStatus int
		wantCode   string
		wantBody   string
	}{
		{name: "none", path: "/api/v1/apps/web/blocks", wantStatus: http.StatusOK, wantBody: `{"blocks":[]}` + "\n"},
		{
			name:       "one",
			path:       "/api/v1/apps/web/blocks",
			listed:     []core.Block{sampleBlock()},
			wantStatus: http.StatusOK,
			wantBody: `{"blocks":[{"id":3,"app":"web","environment":"default","digest":"` + string(badDigest) +
				`","commit":{"repository":"acme/web","sha":"` + badSHA + `"},"reason":"rolled back by user:alice","created_by":"user:alice",` +
				`"deployment_id":7,"rollback_id":9,"created_at":"2026-11-23T12:00:00Z","recorded_in_git":true}]}` + "\n",
		},
		{
			name:       "commit-only without repository",
			path:       "/api/v1/apps/web/blocks",
			listed:     []core.Block{{ID: 4, Target: web, Commit: core.Commit{SHA: badSHA}, Reason: "r", CreatedBy: "user:bob", CreatedAt: createdAt}},
			wantStatus: http.StatusOK,
			wantBody: `{"blocks":[{"id":4,"app":"web","environment":"default","commit":{"sha":"` + badSHA +
				`"},"reason":"r","created_by":"user:bob","created_at":"2026-11-23T12:00:00Z","recorded_in_git":false}]}` + "\n",
		},
		{name: "unknown app", path: "/api/v1/apps/nope/blocks", wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "internal error", path: "/api/v1/apps/web/blocks", listErr: errBoom, wantStatus: http.StatusInternalServerError, wantCode: "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRollbacks{listed: tt.listed, blockErr: tt.listErr}
			rec := do(t, newRollbackHandler(t, f, fakeAuth{}, io.Discard), request{method: http.MethodGet, path: tt.path})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("error code = %q, want %q", code, tt.wantCode)
				}
				return
			}
			if rec.Body.String() != tt.wantBody {
				t.Errorf("body =\n%s\nwant\n%s", rec.Body, tt.wantBody)
			}
		})
	}
}

func TestCreateBlockEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		body       string
		created    bool
		blockErr   error
		wantStatus int
		wantCode   string
		wantRef    core.BlockRef
	}{
		{name: "digest", path: "/api/v1/apps/web/blocks", body: `{"digest":"` + string(badDigest) + `","reason":"leaks"}`, created: true, wantStatus: http.StatusCreated, wantRef: core.BlockRef{Digest: badDigest}},
		{name: "commit", path: "/api/v1/apps/web/blocks", body: `{"commit_sha":"` + badSHA + `","reason":"leaks"}`, created: true, wantStatus: http.StatusCreated, wantRef: core.BlockRef{CommitSHA: badSHA}},
		{name: "both", path: "/api/v1/apps/web/blocks", body: `{"digest":"` + string(badDigest) + `","commit_sha":"` + badSHA + `","reason":"leaks"}`, created: true, wantStatus: http.StatusCreated, wantRef: core.BlockRef{Digest: badDigest, CommitSHA: badSHA}},
		{name: "already blocked", path: "/api/v1/apps/web/blocks", body: `{"digest":"` + string(badDigest) + `","reason":"leaks"}`, wantStatus: http.StatusOK, wantRef: core.BlockRef{Digest: badDigest}},
		{name: "neither", path: "/api/v1/apps/web/blocks", body: `{"reason":"leaks"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "bad digest", path: "/api/v1/apps/web/blocks", body: `{"digest":"latest","reason":"leaks"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "short sha", path: "/api/v1/apps/web/blocks", body: `{"commit_sha":"abc","reason":"leaks"}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{
			name: "service rejects", path: "/api/v1/apps/web/blocks", body: `{"digest":"` + string(badDigest) + `","reason":""}`,
			blockErr: fmt.Errorf("%w: reason is required", core.ErrInvalidBlock), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantRef: core.BlockRef{Digest: badDigest},
		},
		{name: "unknown app", path: "/api/v1/apps/nope/blocks", body: `{"digest":"` + string(badDigest) + `","reason":"leaks"}`, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "internal error", path: "/api/v1/apps/web/blocks", body: `{"digest":"` + string(badDigest) + `","reason":"leaks"}`, blockErr: errBoom, wantStatus: http.StatusInternalServerError, wantCode: "internal", wantRef: core.BlockRef{Digest: badDigest}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRollbacks{created: tt.created, blockErr: tt.blockErr}
			rec := do(t, newRollbackHandler(t, f, fakeAuth{}, io.Discard), request{method: http.MethodPost, path: tt.path, body: tt.body})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("error code = %q, want %q", code, tt.wantCode)
				}
			}
			if tt.wantRef.IsZero() {
				if len(f.blockReqs) != 0 {
					t.Errorf("service called with %+v", f.blockReqs)
				}
				return
			}
			if len(f.blockReqs) != 1 || f.blockReqs[0].Ref != tt.wantRef || f.blockReqs[0].Target != web ||
				f.blockReqs[0].Actor != alice || f.blockReqs[0].Role != core.RoleDeployer {
				t.Fatalf("block requests = %+v, want ref %+v", f.blockReqs, tt.wantRef)
			}
			if tt.wantCode == "" {
				body := decodeBody(t, rec.Body.String())
				if body["created_by"] != "user:alice" || body["reason"] != "leaks" {
					t.Errorf("body = %v", body)
				}
			}
		})
	}
}

func TestClearBlockEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		blockErr   error
		wantStatus int
		wantCode   string
		wantRef    core.BlockRef
	}{
		{name: "digest", path: "/api/v1/apps/web/blocks/" + string(badDigest), wantStatus: http.StatusOK, wantRef: core.BlockRef{Digest: badDigest}},
		{name: "commit", path: "/api/v1/apps/web/blocks/" + badSHA, wantStatus: http.StatusOK, wantRef: core.BlockRef{CommitSHA: badSHA}},
		{name: "app removed from the configuration", path: "/api/v1/apps/gone/blocks/" + badSHA, wantStatus: http.StatusOK, wantRef: core.BlockRef{CommitSHA: badSHA}},
		{name: "not blocked", path: "/api/v1/apps/web/blocks/" + badSHA, blockErr: fmt.Errorf("unblock: %w", core.ErrNotFound), wantStatus: http.StatusNotFound, wantCode: "not_found", wantRef: core.BlockRef{CommitSHA: badSHA}},
		{name: "bad ref", path: "/api/v1/apps/web/blocks/main", wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "bad digest ref", path: "/api/v1/apps/web/blocks/sha256:abc", wantStatus: http.StatusBadRequest, wantCode: "invalid_request"},
		{name: "internal error", path: "/api/v1/apps/web/blocks/" + badSHA, blockErr: errBoom, wantStatus: http.StatusInternalServerError, wantCode: "internal", wantRef: core.BlockRef{CommitSHA: badSHA}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRollbacks{blockErr: tt.blockErr}
			rec := do(t, newRollbackHandler(t, f, fakeAuth{}, io.Discard), request{method: http.MethodDelete, path: tt.path})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("error code = %q, want %q", code, tt.wantCode)
				}
			}
			if tt.wantRef.IsZero() != (len(f.unblocks) == 0) || (len(f.unblocks) == 1 && f.unblocks[0] != tt.wantRef) {
				t.Fatalf("unblocks = %+v, want %+v", f.unblocks, tt.wantRef)
			}
			if tt.wantStatus == http.StatusOK {
				blocks := decodeBody(t, rec.Body.String())["blocks"].([]any)
				if len(blocks) != 1 || blocks[0].(map[string]any)["cleared_by"] != "user:alice" {
					t.Errorf("blocks = %v", blocks)
				}
			}
		})
	}
}

func TestBlockRoutesMethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newRollbackHandler(t, &fakeRollbacks{}, fakeAuth{}, io.Discard)
	tests := []struct {
		method    string
		path      string
		wantAllow string
	}{
		{method: http.MethodDelete, path: "/api/v1/apps/web/blocks", wantAllow: "GET, HEAD, POST"},
		{method: http.MethodGet, path: "/api/v1/apps/web/blocks/" + badSHA, wantAllow: "DELETE"},
		{method: http.MethodPut, path: "/api/v1/apps/web/rollback", wantAllow: "POST"},
	}
	for _, tt := range tests {
		rec := do(t, h, request{method: tt.method, path: tt.path})
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != tt.wantAllow {
			t.Errorf("%s %s = %d Allow %q, want 405 %q", tt.method, tt.path, rec.Code, rec.Header().Get("Allow"), tt.wantAllow)
		}
	}
}
