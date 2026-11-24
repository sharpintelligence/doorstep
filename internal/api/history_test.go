package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/api"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	testSHA    = "0123456789abcdef0123456789abcdef01234567"
	otherSHA   = "89abcdef0123456789abcdef0123456789abcdef"
	testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type fakeHistory struct {
	mu      sync.Mutex
	filters []store.DeploymentFilter
	ids     []core.DeploymentID
	page    store.DeploymentPage
	history store.DeploymentHistory
	err     error
}

func (f *fakeHistory) ListDeployments(_ context.Context, filter store.DeploymentFilter) (store.DeploymentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	return f.page, f.err
}

func (f *fakeHistory) GetDeploymentHistory(_ context.Context, id core.DeploymentID) (store.DeploymentHistory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id)
	return f.history, f.err
}

func serveHistory(t *testing.T, opts api.Options, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody)
	rec := httptest.NewRecorder()
	api.New(opts).ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response %q is not JSON: %v", rec.Body.String(), err)
	}
	return body
}

func assertHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for name, want := range map[string]string{
		"Content-Type":           "application/json",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, status, rec.Body.String())
	}
	assertHeaders(t, rec)
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	if body.Error.Code != code || !strings.Contains(body.Error.Message, message) {
		t.Errorf("error = %+v, want code %s containing %q", body.Error, code, message)
	}
}

func TestListDeploymentsQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		want  store.DeploymentFilter
	}{
		{name: "no parameters", want: store.DeploymentFilter{App: "web"}},
		{name: "environment", query: "environment=prod", want: store.DeploymentFilter{App: "web", Environment: "prod"}},
		{name: "one state", query: "state=failed", want: store.DeploymentFilter{App: "web", States: []core.State{core.StateFailed}}},
		{name: "repeated states", query: "state=failed&state=succeeded", want: store.DeploymentFilter{App: "web", States: []core.State{core.StateFailed, core.StateSucceeded}}},
		{name: "comma separated states", query: "state=queued,resolving&state=queued", want: store.DeploymentFilter{App: "web", States: []core.State{core.StateQueued, core.StateResolving}}},
		{name: "kinds", query: "kind=rollback,deploy", want: store.DeploymentFilter{App: "web", Kinds: []core.Kind{core.KindRollback, core.KindDeploy}}},
		{name: "commit", query: "commit=" + testSHA, want: store.DeploymentFilter{App: "web", CommitSHA: testSHA}},
		{name: "digest", query: "digest=" + testDigest, want: store.DeploymentFilter{App: "web", Digest: testDigest}},
		{name: "limit", query: "limit=200", want: store.DeploymentFilter{App: "web", Limit: 200}},
		{name: "cursor", query: "cursor=42", want: store.DeploymentFilter{App: "web", Before: 42}},
		{name: "everything", query: "environment=staging&state=succeeded&kind=deploy&commit=" + otherSHA + "&digest=" + testDigest + "&limit=1&cursor=7", want: store.DeploymentFilter{
			App: "web", Environment: "staging", States: []core.State{core.StateSucceeded}, Kinds: []core.Kind{core.KindDeploy},
			CommitSHA: otherSHA, Digest: testDigest, Limit: 1, Before: 7,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &fakeHistory{}
			rec := serveHistory(t, api.Options{History: h}, http.MethodGet, "/api/v1/apps/web/deployments?"+tt.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			assertHeaders(t, rec)
			if len(h.filters) != 1 || !reflect.DeepEqual(h.filters[0], tt.want) {
				t.Errorf("filter = %+v, want %+v", h.filters, tt.want)
			}
			if got := rec.Body.String(); got != "{\"items\":[]}\n" {
				t.Errorf("empty list body = %q", got)
			}
		})
	}
}

func TestListDeploymentsRejectsBadQueries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		message string
	}{
		{name: "unknown parameter", path: "/api/v1/apps/web/deployments?status=failed", message: `unknown query parameter "status"`},
		{name: "unknown state", path: "/api/v1/apps/web/deployments?state=paused", message: `"paused"`},
		{name: "uppercase state", path: "/api/v1/apps/web/deployments?state=Failed", message: `"Failed"`},
		{name: "empty state", path: "/api/v1/apps/web/deployments?state=", message: "empty value"},
		{name: "trailing comma", path: "/api/v1/apps/web/deployments?state=failed,", message: "empty value"},
		{name: "unknown kind", path: "/api/v1/apps/web/deployments?kind=hotfix", message: `kind "hotfix"`},
		{name: "short commit", path: "/api/v1/apps/web/deployments?commit=abc1234", message: "commit SHA"},
		{name: "tag instead of digest", path: "/api/v1/apps/web/deployments?digest=latest", message: "digest"},
		{name: "short digest", path: "/api/v1/apps/web/deployments?digest=sha256:abc", message: "digest"},
		{name: "repeated digest", path: "/api/v1/apps/web/deployments?digest=" + testDigest + "&digest=" + testDigest, message: "only be given once"},
		{name: "blank environment", path: "/api/v1/apps/web/deployments?environment=", message: "environment"},
		{name: "repeated environment", path: "/api/v1/apps/web/deployments?environment=a&environment=b", message: "only be given once"},
		{name: "zero limit", path: "/api/v1/apps/web/deployments?limit=0", message: "limit"},
		{name: "large limit", path: "/api/v1/apps/web/deployments?limit=201", message: "limit"},
		{name: "text limit", path: "/api/v1/apps/web/deployments?limit=ten", message: "limit"},
		{name: "negative cursor", path: "/api/v1/apps/web/deployments?cursor=-1", message: "cursor"},
		{name: "zero cursor", path: "/api/v1/apps/web/deployments?cursor=0", message: "cursor"},
		{name: "padded cursor", path: "/api/v1/apps/web/deployments?cursor=007", message: "cursor"},
		{name: "text cursor", path: "/api/v1/apps/web/deployments?cursor=abc", message: "cursor"},
		{name: "blank app", path: "/api/v1/apps/%20/deployments", message: "app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &fakeHistory{}
			rec := serveHistory(t, api.Options{History: h}, http.MethodGet, tt.path)
			assertError(t, rec, http.StatusBadRequest, "invalid_request", tt.message)
			if len(h.filters) != 0 {
				t.Errorf("history queried with %+v", h.filters)
			}
		})
	}
}

func TestGetDeploymentRejectsBadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		message string
	}{
		{name: "text id", path: "/api/v1/deployments/abc", message: "positive integer"},
		{name: "zero id", path: "/api/v1/deployments/0", message: "positive integer"},
		{name: "negative id", path: "/api/v1/deployments/-3", message: "positive integer"},
		{name: "signed id", path: "/api/v1/deployments/+3", message: "positive integer"},
		{name: "overflowing id", path: "/api/v1/deployments/9223372036854775808", message: "positive integer"},
		{name: "query parameter", path: "/api/v1/deployments/3?verbose=1", message: "no query parameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &fakeHistory{}
			rec := serveHistory(t, api.Options{History: h}, http.MethodGet, tt.path)
			assertError(t, rec, http.StatusBadRequest, "invalid_request", tt.message)
			if len(h.ids) != 0 {
				t.Errorf("history queried with %v", h.ids)
			}
		})
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		err     error
		status  int
		code    string
		message string
		logged  bool
	}{
		{name: "detail not found", path: "/api/v1/deployments/9", err: fmt.Errorf("deployment 9: %w", core.ErrNotFound), status: http.StatusNotFound, code: "not_found", message: "deployment 9 not found"},
		{name: "detail failure", path: "/api/v1/deployments/9", err: errors.New("connection refused to db.internal"), status: http.StatusInternalServerError, code: "internal", message: "internal server error", logged: true},
		{name: "list failure", path: "/api/v1/apps/web/deployments", err: errors.New("connection refused to db.internal"), status: http.StatusInternalServerError, code: "internal", message: "internal server error", logged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			rec := serveHistory(t, api.Options{History: &fakeHistory{err: tt.err}, Logger: logger}, http.MethodGet, tt.path)
			assertError(t, rec, tt.status, tt.code, tt.message)
			if strings.Contains(rec.Body.String(), "db.internal") {
				t.Errorf("response leaks the error: %s", rec.Body.String())
			}
			if got := strings.Contains(logs.String(), "db.internal"); got != tt.logged {
				t.Errorf("error logged = %v, want %v: %s", got, tt.logged, logs.String())
			}
		})
	}
}

func fixture() (core.Deployment, core.Deployment) {
	created := time.Date(2026, 3, 1, 12, 0, 0, 0, time.FixedZone("CET", 3600))
	started := created.Add(time.Minute)
	finished := created.Add(5 * time.Minute)
	parent := core.DeploymentID(3)
	superseded := core.DeploymentID(12)
	full := core.Deployment{
		ID:               10,
		Target:           core.Target{App: "web", Environment: "prod"},
		Kind:             core.KindDeploy,
		State:            core.StateFailed,
		Source:           core.SourceWebhook,
		RequestedBy:      "octocat",
		Commit:           core.Commit{Repository: "acme/web", SHA: testSHA},
		Image:            core.ImageRef{Repository: "ghcr.io/acme/web", Tag: testSHA},
		Digest:           testDigest,
		PreviousDigest:   testDigest,
		PinCommitSHA:     otherSHA,
		GitOpsRepository: "acme/gitops",
		ArgoCDRevision:   otherSHA,
		ParentID:         &parent,
		SupersededByID:   &superseded,
		FailureReason:    "health gate failed",
		CreatedAt:        created,
		UpdatedAt:        finished,
		StartedAt:        &started,
		FinishedAt:       &finished,
	}
	origin := core.DeploymentID(10)
	minimal := core.Deployment{
		ID:           11,
		Target:       core.Target{App: "web", Environment: "prod"},
		Kind:         core.KindRollback,
		State:        core.StateQueued,
		Source:       core.SourceSystem,
		Commit:       core.Commit{Repository: "acme/web", SHA: testSHA},
		Image:        core.ImageRef{Repository: "ghcr.io/acme/web", Tag: testSHA},
		RollbackOfID: &origin,
		CreatedAt:    created.UTC(),
		UpdatedAt:    created.UTC(),
	}
	return full, minimal
}

const fullJSON = `{
	"id": 10, "app": "web", "environment": "prod", "kind": "deploy", "state": "failed",
	"trigger": {"source": "webhook", "requested_by": "octocat"},
	"commit": {"repository": "acme/web", "sha": "` + testSHA + `"},
	"image": {"repository": "ghcr.io/acme/web", "tag": "` + testSHA + `", "digest": "` + testDigest + `", "previous_digest": "` + testDigest + `"},
	"gitops": {"repository": "acme/gitops", "commit_sha": "` + otherSHA + `"},
	"argocd": {"revision": "` + otherSHA + `"},
	"parent_id": 3, "superseded_by_id": 12, "failure_reason": "health gate failed",
	"created_at": "2026-03-01T11:00:00Z", "updated_at": "2026-03-01T11:05:00Z",
	"started_at": "2026-03-01T11:01:00Z", "finished_at": "2026-03-01T11:05:00Z"
}`

const minimalJSON = `{
	"id": 11, "app": "web", "environment": "prod", "kind": "rollback", "state": "queued",
	"trigger": {"source": "system"},
	"commit": {"repository": "acme/web", "sha": "` + testSHA + `"},
	"image": {"repository": "ghcr.io/acme/web", "tag": "` + testSHA + `"},
	"gitops": {}, "argocd": {}, "rollback_of_id": 10,
	"created_at": "2026-03-01T11:00:00Z", "updated_at": "2026-03-01T11:00:00Z"
}`

func assertJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("response %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("expected JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got  %s\n want %s", got, want)
	}
}

func TestListDeploymentsResponse(t *testing.T) {
	t.Parallel()

	full, minimal := fixture()
	tests := []struct {
		name string
		page store.DeploymentPage
		want string
	}{
		{name: "with next page", page: store.DeploymentPage{Items: []core.Deployment{full, minimal}, Next: 11}, want: `{"items": [` + fullJSON + `,` + minimalJSON + `], "next_cursor": "11"}`},
		{name: "last page", page: store.DeploymentPage{Items: []core.Deployment{minimal}}, want: `{"items": [` + minimalJSON + `]}`},
		{name: "empty", want: `{"items": []}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := serveHistory(t, api.Options{History: &fakeHistory{page: tt.page}}, http.MethodGet, "/api/v1/apps/web/deployments")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			assertJSON(t, rec.Body.Bytes(), tt.want)
		})
	}
}

func TestListDeploymentsCursorRoundTrip(t *testing.T) {
	t.Parallel()

	full, _ := fixture()
	h := &fakeHistory{page: store.DeploymentPage{Items: []core.Deployment{full}, Next: full.ID}}
	rec := serveHistory(t, api.Options{History: h}, http.MethodGet, "/api/v1/apps/web/deployments?limit=1&environment=prod")
	cursor, ok := decode(t, rec)["next_cursor"].(string)
	if !ok {
		t.Fatalf("no next_cursor in %s", rec.Body.String())
	}
	serveHistory(t, api.Options{History: h}, http.MethodGet, "/api/v1/apps/web/deployments?limit=1&environment=prod&cursor="+cursor)
	if len(h.filters) != 2 || h.filters[1].Before != full.ID || h.filters[1].Limit != 1 || h.filters[1].Environment != "prod" {
		t.Errorf("filters = %+v, want the second to continue before %d", h.filters, full.ID)
	}
}

func TestGetDeploymentResponse(t *testing.T) {
	t.Parallel()

	full, minimal := fixture()
	occurred := time.Date(2026, 3, 1, 12, 0, 0, 0, time.FixedZone("CET", 3600))
	gateDone := occurred.Add(time.Minute)
	tests := []struct {
		name    string
		history store.DeploymentHistory
		want    string
	}{
		{
			name: "full",
			history: store.DeploymentHistory{
				Deployment: full,
				Events: []core.Event{
					{ID: 1, DeploymentID: 10, Type: core.EventTransition, To: core.StateQueued, Actor: "octocat", Message: "queued from webhook", Data: json.RawMessage(`{}`), OccurredAt: occurred},
					{ID: 2, DeploymentID: 10, Type: core.EventTransition, From: core.StateVerifying, To: core.StateFailed, Data: json.RawMessage(`{"argocd_revision":"` + otherSHA + `"}`), OccurredAt: occurred},
					{ID: 3, DeploymentID: 10, Type: core.EventNote, OccurredAt: occurred},
				},
				EventsTruncated: true,
				Gates: []core.GateResult{
					{Name: "health", Status: core.GateFailed, Message: "degraded", Evidence: json.RawMessage(`{"status":"Degraded"}`), StartedAt: occurred, FinishedAt: &gateDone},
					{Name: "soak", Status: core.GatePending, StartedAt: occurred},
				},
				Rollbacks: []core.Deployment{minimal},
			},
			want: `{
				"deployment": ` + fullJSON + `,
				"events": [
					{"id": 1, "type": "transition", "to": "queued", "actor": "octocat", "message": "queued from webhook", "data": {}, "occurred_at": "2026-03-01T11:00:00Z"},
					{"id": 2, "type": "transition", "from": "verifying", "to": "failed", "data": {"argocd_revision": "` + otherSHA + `"}, "occurred_at": "2026-03-01T11:00:00Z"},
					{"id": 3, "type": "note", "data": {}, "occurred_at": "2026-03-01T11:00:00Z"}
				],
				"events_truncated": true,
				"gates": [
					{"name": "health", "status": "failed", "message": "degraded", "evidence": {"status": "Degraded"}, "started_at": "2026-03-01T11:00:00Z", "finished_at": "2026-03-01T11:01:00Z"},
					{"name": "soak", "status": "pending", "evidence": {}, "started_at": "2026-03-01T11:00:00Z"}
				],
				"rollbacks": [` + minimalJSON + `],
				"retries": []
			}`,
		},
		{
			name:    "no related records",
			history: store.DeploymentHistory{Deployment: minimal},
			want:    `{"deployment": ` + minimalJSON + `, "events": [], "events_truncated": false, "gates": [], "rollbacks": [], "retries": []}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &fakeHistory{history: tt.history}
			rec := serveHistory(t, api.Options{History: h}, http.MethodGet, "/api/v1/deployments/10")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			assertHeaders(t, rec)
			if len(h.ids) != 1 || h.ids[0] != 10 {
				t.Errorf("requested ids = %v, want [10]", h.ids)
			}
			assertJSON(t, rec.Body.Bytes(), tt.want)
		})
	}
}

func TestHistoryRoutes(t *testing.T) {
	t.Parallel()

	var wrapped []string
	middleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped = append(wrapped, r.URL.Path)
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	for _, path := range []string{"/api/v1/apps/web/deployments", "/api/v1/deployments/1"} {
		rec := serveHistory(t, api.Options{History: &fakeHistory{}, Middleware: middleware}, http.MethodGet, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without credentials = %d, want 401", path, rec.Code)
		}
	}
	if len(wrapped) != 2 {
		t.Errorf("middleware saw %v, want both routes", wrapped)
	}

	tests := []struct {
		name    string
		history api.History
		method  string
		path    string
		want    int
	}{
		{name: "post to the list", history: &fakeHistory{}, method: http.MethodPost, path: "/api/v1/apps/web/deployments", want: http.StatusMethodNotAllowed},
		{name: "delete a deployment", history: &fakeHistory{}, method: http.MethodDelete, path: "/api/v1/deployments/1", want: http.StatusMethodNotAllowed},
		{name: "head a deployment", history: &fakeHistory{}, method: http.MethodHead, path: "/api/v1/deployments/1", want: http.StatusOK},
		{name: "app without a resource", history: &fakeHistory{}, method: http.MethodGet, path: "/api/v1/apps/web", want: http.StatusNotFound},
		{name: "unknown sub-resource", history: &fakeHistory{}, method: http.MethodGet, path: "/api/v1/deployments/1/events", want: http.StatusNotFound},
		{name: "history not configured", method: http.MethodGet, path: "/api/v1/deployments/1", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := serveHistory(t, api.Options{History: tt.history}, tt.method, tt.path)
			if rec.Code != tt.want {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
			}
		})
	}
}
