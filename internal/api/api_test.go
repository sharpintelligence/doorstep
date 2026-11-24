package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const goodToken = "Bearer good-token"

var (
	errBoom   = errors.New("database password=hunter2 unreachable")
	createdAt = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	expiresAt = time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)
	web       = core.Target{App: "web", Environment: core.DefaultEnvironment}
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(r *http.Request) (string, error) {
	if r.Header.Get("Authorization") == goodToken {
		return "alice", nil
	}
	return "", ErrUnauthenticated
}

type emptyActorAuth struct{}

func (emptyActorAuth) Authenticate(*http.Request) (string, error) { return "", nil }

type fakeLocks struct {
	mu        sync.Mutex
	lockReqs  []core.LockRequest
	unlocks   []string
	lockErr   error
	unlockErr error
	listErr   error
	listed    []core.Lock
}

func (f *fakeLocks) Lock(_ context.Context, req core.LockRequest) (core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lockReqs = append(f.lockReqs, req)
	if f.lockErr != nil {
		return core.Lock{}, f.lockErr
	}
	l := core.Lock{Global: req.Target == nil, Kind: core.LockFreeze, Owner: req.Owner, Reason: req.Reason, CreatedAt: createdAt}
	if req.Target != nil {
		l.Target = *req.Target
	}
	if req.ExpiresIn != nil {
		at := createdAt.Add(*req.ExpiresIn)
		l.ExpiresAt = &at
	}
	if req.ExpiresAt != nil {
		l.ExpiresAt = req.ExpiresAt
	}
	return l, nil
}

func (f *fakeLocks) Unlock(_ context.Context, target *core.Target, actor string) (core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlocks = append(f.unlocks, core.LockRequest{Target: target}.Scope()+" by "+actor)
	return core.Lock{}, f.unlockErr
}

func (f *fakeLocks) List(context.Context) ([]core.Lock, error) {
	return f.listed, f.listErr
}

func knownApp(app string) bool {
	return app == "web" || app == "api"
}

func newHandler(t *testing.T, locks LockService, auth Authenticator, logs io.Writer) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(logs, nil))
	return New(Options{Locks: locks, KnownApp: knownApp, Authenticator: auth, Logger: logger})
}

type request struct {
	method      string
	path        string
	body        string
	contentType string
	noAuth      bool
}

func do(t *testing.T, h http.Handler, req request) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader = http.NoBody
	if req.body != "" {
		body = strings.NewReader(req.body)
	}
	r := httptest.NewRequestWithContext(t.Context(), req.method, req.path, body)
	if !req.noAuth {
		r.Header.Set("Authorization", goodToken)
	}
	switch {
	case req.contentType != "":
		r.Header.Set("Content-Type", req.contentType)
	case req.body != "":
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	if body.Error.Message == "" {
		t.Errorf("error without message: %s", rec.Body.String())
	}
	return body.Error.Code
}

func TestLockRoutesNeedEveryDependency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts Options
	}{
		{"nothing", Options{}},
		{"no locks", Options{KnownApp: knownApp, Authenticator: RejectAll{}}},
		{"no app lookup", Options{Locks: &fakeLocks{}, Authenticator: RejectAll{}}},
		{"no authenticator", Options{Locks: &fakeLocks{}, KnownApp: knownApp}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := do(t, New(tt.opts), request{method: http.MethodGet, path: "/api/v1/locks"})
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET /api/v1/locks = %d, want 404", rec.Code)
			}
		})
	}
}

type emptyQueue struct{}

func (emptyQueue) Entries(context.Context, string) ([]core.QueueEntry, error) { return nil, nil }

type knownApps map[string]bool

func (k knownApps) Known(app string) bool { return k[app] }

func TestLockRoutesCoexistWithQueueRoutes(t *testing.T) {
	t.Parallel()

	h := New(Options{
		Queue:         emptyQueue{},
		Apps:          knownApps{"web": true},
		Locks:         &fakeLocks{},
		KnownApp:      knownApp,
		Authenticator: fakeAuth{},
	})
	for _, path := range []string{"/api/v1/locks", "/api/v1/apps/web/queue"} {
		if rec := do(t, h, request{method: http.MethodGet, path: path}); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()

	requests := []request{
		{method: http.MethodPost, path: "/api/v1/apps/web/lock", body: `{"reason":"r"}`},
		{method: http.MethodDelete, path: "/api/v1/apps/web/lock"},
		{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r"}`},
		{method: http.MethodDelete, path: "/api/v1/lock"},
		{method: http.MethodGet, path: "/api/v1/locks"},
		{method: http.MethodPost, path: "/api/v1/apps/unknown/lock", body: `{"reason":"r"}`},
	}
	tests := []struct {
		name   string
		auth   Authenticator
		header string
	}{
		{name: "reject all", auth: RejectAll{}, header: goodToken},
		{name: "missing token", auth: fakeAuth{}},
		{name: "wrong token", auth: fakeAuth{}, header: "Bearer nope"},
		{name: "empty actor", auth: emptyActorAuth{}, header: goodToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			locks := &fakeLocks{}
			h := newHandler(t, locks, tt.auth, io.Discard)
			for _, req := range requests {
				req.noAuth = true
				r := httptest.NewRequestWithContext(t.Context(), req.method, req.path, strings.NewReader(req.body))
				r.Header.Set("Content-Type", "application/json")
				if tt.header != "" {
					r.Header.Set("Authorization", tt.header)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s %s = %d, want 401", req.method, req.path, rec.Code)
					continue
				}
				if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
					t.Errorf("%s %s WWW-Authenticate = %q", req.method, req.path, got)
				}
				if code := errorCode(t, rec); code != "unauthorized" {
					t.Errorf("%s %s code = %q", req.method, req.path, code)
				}
			}
			if len(locks.lockReqs) != 0 || len(locks.unlocks) != 0 {
				t.Errorf("unauthenticated requests reached the service: %v %v", locks.lockReqs, locks.unlocks)
			}
		})
	}
}

func TestLock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		req        request
		lockErr    error
		wantStatus int
		wantCode   string
		wantBody   map[string]any
		wantReq    func(t *testing.T, req core.LockRequest)
	}{
		{
			name:       "app lock",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/lock", body: `{"reason":"database migration"}`},
			wantStatus: http.StatusCreated,
			wantBody:   map[string]any{"scope": "app", "app": "web", "owner": "alice", "reason": "database migration", "created_at": "2026-11-23T12:00:00Z", "expires_at": nil},
			wantReq: func(t *testing.T, req core.LockRequest) {
				t.Helper()
				if req.Target == nil || *req.Target != web || req.Owner != "alice" || req.ExpiresIn != nil || req.ExpiresAt != nil {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{
			name:       "global lock with expires_in",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"holiday","expires_in":"2h"}`, contentType: "application/json; charset=utf-8"},
			wantStatus: http.StatusCreated,
			wantBody:   map[string]any{"scope": "global", "owner": "alice", "reason": "holiday", "created_at": "2026-11-23T12:00:00Z", "expires_at": "2026-11-23T14:00:00Z"},
			wantReq: func(t *testing.T, req core.LockRequest) {
				t.Helper()
				if req.Target != nil || req.ExpiresIn == nil || *req.ExpiresIn != 2*time.Hour {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{
			name:       "expires_at",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/api/lock", body: `{"reason":"r","expires_at":"2026-11-23T16:00:00+02:00"}`},
			wantStatus: http.StatusCreated,
			wantBody:   map[string]any{"scope": "app", "app": "api", "owner": "alice", "reason": "r", "created_at": "2026-11-23T12:00:00Z", "expires_at": "2026-11-23T14:00:00Z"},
			wantReq: func(t *testing.T, req core.LockRequest) {
				t.Helper()
				if req.ExpiresAt == nil || !req.ExpiresAt.Equal(expiresAt) || req.ExpiresIn != nil {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{
			name:       "owner field is not accepted",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r","owner":"mallory"}`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "unknown app",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/nope/lock", body: `{"reason":"r"}`},
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "both expiries",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r","expires_in":"1h","expires_at":"2026-11-23T16:00:00Z"}`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "bad duration",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r","expires_in":"2"}`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "bad timestamp",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r","expires_at":"tomorrow"}`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "wrong field type",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":42}`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "malformed JSON",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "empty body",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", contentType: "application/json"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "trailing data",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r"} {"reason":"s"}`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "array body",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `[]`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "missing content type",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r"}`, contentType: " "},
			wantStatus: http.StatusUnsupportedMediaType,
			wantCode:   "unsupported_media_type",
		},
		{
			name:       "form content type",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/lock", body: `reason=r`, contentType: "application/x-www-form-urlencoded"},
			wantStatus: http.StatusUnsupportedMediaType,
			wantCode:   "unsupported_media_type",
		},
		{
			name:       "oversized body",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"` + strings.Repeat("x", MaxRequestBytes) + `"}`},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "invalid_request",
		},
		{
			name:       "oversized trailing data",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r"}` + strings.Repeat(" ", MaxRequestBytes)},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "invalid_request",
		},
		{
			name:       "validation error",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":""}`},
			lockErr:    errors.Join(core.ErrInvalidLock, errors.New("reason is required")),
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "contended",
			req:        request{method: http.MethodPost, path: "/api/v1/apps/web/lock", body: `{"reason":"r"}`},
			lockErr:    fmt.Errorf("lock web: %w", core.ErrLockContended),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "contended",
		},
		{
			name:       "internal error",
			req:        request{method: http.MethodPost, path: "/api/v1/lock", body: `{"reason":"r"}`},
			lockErr:    errBoom,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			locks := &fakeLocks{lockErr: tt.lockErr}
			var logs bytes.Buffer
			rec := do(t, newHandler(t, locks, fakeAuth{}, &logs), tt.req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("response leaks internal error: %s", rec.Body.String())
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			}
			if tt.wantStatus == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Error("503 without Retry-After")
			}
			if tt.wantCode == "internal" && !strings.Contains(logs.String(), "hunter2") {
				t.Errorf("internal error not logged: %s", logs.String())
			}
			if tt.wantBody != nil {
				var got map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("body: %v", err)
				}
				if len(got) != len(tt.wantBody) {
					t.Errorf("body = %v, want %v", got, tt.wantBody)
				}
				for k, v := range tt.wantBody {
					if got[k] != v {
						t.Errorf("body[%q] = %v, want %v", k, got[k], v)
					}
				}
			}
			if tt.wantReq != nil {
				if len(locks.lockReqs) != 1 {
					t.Fatalf("service called %d times", len(locks.lockReqs))
				}
				tt.wantReq(t, locks.lockReqs[0])
			}
			if tt.wantStatus >= 400 && tt.lockErr == nil && len(locks.lockReqs) != 0 {
				t.Errorf("rejected request reached the service: %+v", locks.lockReqs)
			}
		})
	}
}

func TestLockConflictReturnsHolder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		held core.Lock
		want lockResponse
	}{
		{
			name: "app",
			path: "/api/v1/apps/web/lock",
			held: core.Lock{Target: web, Owner: "bob", Reason: "first", CreatedAt: createdAt, ExpiresAt: &expiresAt},
			want: lockResponse{Scope: "app", App: "web", Owner: "bob", Reason: "first", CreatedAt: createdAt, ExpiresAt: &expiresAt},
		},
		{
			name: "global",
			path: "/api/v1/lock",
			held: core.Lock{Global: true, Owner: "bob", Reason: "holiday", CreatedAt: createdAt},
			want: lockResponse{Scope: "global", Owner: "bob", Reason: "holiday", CreatedAt: createdAt},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			locks := &fakeLocks{lockErr: &core.LockHeldError{Lock: tt.held}}
			rec := do(t, newHandler(t, locks, fakeAuth{}, io.Discard), request{method: http.MethodPost, path: tt.path, body: `{"reason":"second"}`})
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			var body lockConflictResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body: %v", err)
			}
			if body.Error.Code != "locked" || !strings.Contains(body.Error.Message, "bob") {
				t.Errorf("error = %+v", body.Error)
			}
			if body.Lock.Scope != tt.want.Scope || body.Lock.App != tt.want.App || body.Lock.Owner != tt.want.Owner ||
				body.Lock.Reason != tt.want.Reason || !body.Lock.CreatedAt.Equal(tt.want.CreatedAt) ||
				(body.Lock.ExpiresAt == nil) != (tt.want.ExpiresAt == nil) {
				t.Errorf("lock = %+v, want %+v", body.Lock, tt.want)
			}
		})
	}
}

func TestUnlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		unlockErr   error
		wantStatus  int
		wantCode    string
		wantUnlocks []string
	}{
		{name: "app", path: "/api/v1/apps/web/lock", wantStatus: http.StatusNoContent, wantUnlocks: []string{"web by alice"}},
		{name: "global", path: "/api/v1/lock", wantStatus: http.StatusNoContent, wantUnlocks: []string{"global by alice"}},
		{name: "not locked", path: "/api/v1/apps/web/lock", unlockErr: core.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found", wantUnlocks: []string{"web by alice"}},
		{name: "global not locked", path: "/api/v1/lock", unlockErr: core.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found", wantUnlocks: []string{"global by alice"}},
		{name: "app removed from the configuration", path: "/api/v1/apps/gone/lock", wantStatus: http.StatusNoContent, wantUnlocks: []string{"gone by alice"}},
		{name: "unknown app not locked", path: "/api/v1/apps/nope/lock", unlockErr: core.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found", wantUnlocks: []string{"nope by alice"}},
		{name: "invalid", path: "/api/v1/lock", unlockErr: core.ErrInvalidLock, wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantUnlocks: []string{"global by alice"}},
		{name: "internal error", path: "/api/v1/lock", unlockErr: errBoom, wantStatus: http.StatusInternalServerError, wantCode: "internal", wantUnlocks: []string{"global by alice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			locks := &fakeLocks{unlockErr: tt.unlockErr}
			rec := do(t, newHandler(t, locks, fakeAuth{}, io.Discard), request{method: http.MethodDelete, path: tt.path})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				if code := errorCode(t, rec); code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			} else if rec.Body.Len() != 0 {
				t.Errorf("204 with body %q", rec.Body.String())
			}
			if strings.Join(locks.unlocks, ",") != strings.Join(tt.wantUnlocks, ",") {
				t.Errorf("unlocks = %q, want %q", locks.unlocks, tt.wantUnlocks)
			}
		})
	}
}

func TestListLocks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		locks      *fakeLocks
		wantStatus int
		wantBody   string
	}{
		{
			name:       "empty",
			locks:      &fakeLocks{},
			wantStatus: http.StatusOK,
			wantBody:   `{"locks":[]}`,
		},
		{
			name: "global and app",
			locks: &fakeLocks{listed: []core.Lock{
				{Global: true, Owner: "bob", Reason: "holiday", CreatedAt: createdAt},
				{Target: web, Owner: "alice", Reason: "migration", CreatedAt: createdAt.In(time.FixedZone("x", 3600)), ExpiresAt: &expiresAt},
			}},
			wantStatus: http.StatusOK,
			wantBody: `{"locks":[` +
				`{"scope":"global","owner":"bob","reason":"holiday","created_at":"2026-11-23T12:00:00Z","expires_at":null},` +
				`{"scope":"app","app":"web","owner":"alice","reason":"migration","created_at":"2026-11-23T12:00:00Z","expires_at":"2026-11-23T14:00:00Z"}]}`,
		},
		{
			name:       "internal error",
			locks:      &fakeLocks{listErr: errBoom},
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":{"code":"internal","message":"internal error"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, newHandler(t, tt.locks, fakeAuth{}, io.Discard), request{method: http.MethodGet, path: "/api/v1/locks"})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tt.wantBody {
				t.Errorf("body =\n%s\nwant\n%s", got, tt.wantBody)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()

	h := newHandler(t, &fakeLocks{}, fakeAuth{}, io.Discard)
	rec := do(t, h, request{method: http.MethodPut, path: "/api/v1/lock", body: `{"reason":"r"}`})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/v1/lock = %d, want 405", rec.Code)
	}
}
