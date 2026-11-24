package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/api"
	"github.com/sharpintelligence/doorstep/internal/core"
)

type fakeQueue struct {
	entries map[string][]core.QueueEntry
	err     error
	calls   []string
}

func (f *fakeQueue) Entries(_ context.Context, app string) ([]core.QueueEntry, error) {
	f.calls = append(f.calls, app)
	return f.entries[app], f.err
}

type fakeApps map[string]bool

func (f fakeApps) Known(app string) bool { return f[app] }

const sha = "0123456789abcdef0123456789abcdef01234567"

func sampleEntries() []core.QueueEntry {
	enqueued := time.Date(2026, 11, 1, 12, 0, 0, 0, time.FixedZone("MST", -7*3600))
	lease := enqueued.Add(time.Minute)
	deployment := func(id core.DeploymentID, state core.State) core.Deployment {
		return core.Deployment{
			ID:          id,
			Target:      core.Target{App: "web", Environment: "prod"},
			Kind:        core.KindDeploy,
			State:       state,
			Source:      core.SourceWebhook,
			RequestedBy: "octocat",
			Commit:      core.Commit{Repository: "acme/web", SHA: sha},
		}
	}
	return []core.QueueEntry{
		{Position: 1, Deployment: deployment(7, core.StateSyncing), Priority: core.PriorityNormal, EnqueuedAt: enqueued, AvailableAt: enqueued, LeaseExpiresAt: &lease, Attempts: 3},
		{Position: 2, Deployment: deployment(9, core.StateQueued), Priority: core.PriorityUrgent, EnqueuedAt: enqueued, AvailableAt: enqueued},
	}
}

func serve(t *testing.T, h http.Handler, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if method != http.MethodHead && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s body %q is not JSON: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec, body
}

func TestGetQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		method     string
		queue      *fakeQueue
		apps       fakeApps
		wantStatus int
		wantCode   string
		wantDepth  float64
		wantCount  int
	}{
		{
			name:       "entries in claim order",
			path:       "/api/v1/apps/web/queue",
			queue:      &fakeQueue{entries: map[string][]core.QueueEntry{"web": sampleEntries()}},
			apps:       fakeApps{"web": true},
			wantStatus: http.StatusOK,
			wantDepth:  1,
			wantCount:  2,
		},
		{
			name:       "configured app with an empty queue",
			path:       "/api/v1/apps/web/queue",
			queue:      &fakeQueue{},
			apps:       fakeApps{"web": true},
			wantStatus: http.StatusOK,
		},
		{
			name:       "removed app with leftover entries",
			path:       "/api/v1/apps/web/queue",
			queue:      &fakeQueue{entries: map[string][]core.QueueEntry{"web": sampleEntries()}},
			apps:       fakeApps{},
			wantStatus: http.StatusOK,
			wantDepth:  1,
			wantCount:  2,
		},
		{
			name:       "unknown app",
			path:       "/api/v1/apps/nope/queue",
			queue:      &fakeQueue{},
			apps:       fakeApps{"web": true},
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "unknown app with markup in its name",
			path:       "/api/v1/apps/%3Cscript%3Ealert(1)%3C%2Fscript%3E/queue",
			queue:      &fakeQueue{},
			apps:       fakeApps{"web": true},
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "store error hides detail",
			path:       "/api/v1/apps/web/queue",
			queue:      &fakeQueue{err: errors.New("dial tcp 10.0.0.5:5432: password authentication failed")},
			apps:       fakeApps{"web": true},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name:       "method not allowed",
			path:       "/api/v1/apps/web/queue",
			method:     http.MethodPost,
			queue:      &fakeQueue{},
			apps:       fakeApps{"web": true},
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "unknown endpoint",
			path:       "/api/v1/apps/web/history",
			queue:      &fakeQueue{},
			apps:       fakeApps{"web": true},
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "api root",
			path:       "/api/v1/",
			queue:      &fakeQueue{},
			apps:       fakeApps{},
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			h := api.New(api.Options{
				Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
				Queue:  tt.queue,
				Apps:   tt.apps,
			})
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			rec, body := serve(t, h, method, tt.path)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q", cc)
			}
			if tt.wantCode != "" {
				envelope, _ := body["error"].(map[string]any)
				if envelope["code"] != tt.wantCode || envelope["message"] == "" {
					t.Errorf("error = %v, want code %q with a message", body["error"], tt.wantCode)
				}
				if strings.Contains(rec.Body.String(), "nope") || strings.Contains(rec.Body.String(), "script") {
					t.Errorf("response echoes the requested app: %s", rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "password") {
					t.Errorf("response leaks the store error: %s", rec.Body.String())
				}
				if tt.wantStatus == http.StatusInternalServerError && !strings.Contains(logs.String(), "password authentication failed") {
					t.Errorf("store error not logged: %s", logs.String())
				}
				if tt.wantStatus == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != "GET, HEAD" {
					t.Errorf("Allow = %q", rec.Header().Get("Allow"))
				}
				return
			}
			entries, ok := body["entries"].([]any)
			if !ok || len(entries) != tt.wantCount {
				t.Fatalf("entries = %v, want %d", body["entries"], tt.wantCount)
			}
			if body["app"] != "web" || body["depth"] != tt.wantDepth {
				t.Errorf("app %v depth %v, want web and %v", body["app"], body["depth"], tt.wantDepth)
			}
		})
	}
}

func TestGetQueueEntryFields(t *testing.T) {
	t.Parallel()

	h := api.New(api.Options{
		Queue: &fakeQueue{entries: map[string][]core.QueueEntry{"web": sampleEntries()}},
		Apps:  fakeApps{"web": true},
	})
	rec, _ := serve(t, h, http.MethodGet, "/api/v1/apps/web/queue")
	want := `{"app":"web","depth":1,"entries":[` +
		`{"position":1,"deployment_id":7,"environment":"prod","kind":"deploy","state":"syncing","priority":"normal",` +
		`"commit_sha":"` + sha + `","source":"webhook","requested_by":"octocat",` +
		`"enqueued_at":"2026-11-01T19:00:00Z","available_at":"2026-11-01T19:00:00Z","attempts":3,"in_flight":true,` +
		`"lease_expires_at":"2026-11-01T19:01:00Z"},` +
		`{"position":2,"deployment_id":9,"environment":"prod","kind":"deploy","state":"queued","priority":"urgent",` +
		`"commit_sha":"` + sha + `","source":"webhook","requested_by":"octocat",` +
		`"enqueued_at":"2026-11-01T19:00:00Z","available_at":"2026-11-01T19:00:00Z","attempts":0,"in_flight":false,` +
		`"lease_expires_at":null}]}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(rec.Body.String(), "lease_owner") {
		t.Error("response exposes the lease owner")
	}
}

func TestGetQueueHead(t *testing.T) {
	t.Parallel()

	h := api.New(api.Options{Queue: &fakeQueue{}, Apps: fakeApps{"web": true}})
	rec, _ := serve(t, h, http.MethodHead, "/api/v1/apps/web/queue")
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD status = %d", rec.Code)
	}
}

func TestMiddlewareWrapsEveryRoute(t *testing.T) {
	t.Parallel()

	q := &fakeQueue{}
	h := api.New(api.Options{
		Queue: q,
		Apps:  fakeApps{"web": true},
		Middleware: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer ok" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	for _, path := range []string{"/api/v1/apps/web/queue", "/api/v1/unknown"} {
		rec, _ := serve(t, h, http.MethodGet, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials = %d, want 401", path, rec.Code)
		}
	}
	if len(q.calls) != 0 {
		t.Errorf("queue read %v before the middleware allowed it", q.calls)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/apps/web/queue", nil)
	req.Header.Set("Authorization", "Bearer ok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authorised GET = %d", rec.Code)
	}
}

func TestNewWithoutQueueServesOnlyNotFound(t *testing.T) {
	t.Parallel()

	rec, body := serve(t, api.New(api.Options{}), http.MethodGet, "/api/v1/apps/web/queue")
	if rec.Code != http.StatusNotFound || body["error"] == nil {
		t.Errorf("GET without a queue = %d %v", rec.Code, body)
	}
}
