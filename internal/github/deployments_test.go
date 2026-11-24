package github

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

type recordedRequest struct {
	method string
	path   string
	query  string
	body   map[string]any
}

type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *recorder) handler(t *testing.T, status int, response string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		rec := recordedRequest{method: req.Method, path: req.URL.EscapedPath(), query: req.URL.RawQuery}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &rec.body); err != nil {
				t.Errorf("request body is not JSON: %v", err)
			}
		}
		r.mu.Lock()
		r.requests = append(r.requests, rec)
		r.mu.Unlock()
		if status >= 300 {
			githubtest.WriteError(w, status, response)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	})
}

func (r *recorder) only(t *testing.T) recordedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(r.requests))
	}
	return r.requests[0]
}

func deploymentsFixture(t *testing.T, pattern string, status int, response string) (*Client, *recorder) {
	t.Helper()
	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	rec := &recorder{}
	f.srv.Handle(pattern, rec.handler(t, status, response))
	return f.repoClient(t, "acme", "app"), rec
}

func TestListDeployments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		filter    DeploymentFilter
		status    int
		response  string
		wantQuery string
		want      []Deployment
		wantErr   int
	}{
		{
			name:      "filtered",
			filter:    DeploymentFilter{SHA: testSHA, Environment: "blog prod", Task: "deploy"},
			status:    http.StatusOK,
			response:  `[{"id":11,"sha":"` + testSHA + `","ref":"` + testSHA + `","task":"deploy","environment":"blog prod","payload":{"doorstep_deployment_id":42}}]`,
			wantQuery: "environment=blog+prod&per_page=100&sha=" + testSHA + "&task=deploy",
			want: []Deployment{{
				ID: 11, SHA: testSHA, Ref: testSHA, Task: "deploy", Environment: "blog prod",
				Payload: json.RawMessage(`{"doorstep_deployment_id":42}`),
			}},
		},
		{
			name:      "no filter",
			status:    http.StatusOK,
			response:  `[]`,
			wantQuery: "per_page=100",
			want:      []Deployment{},
		},
		{
			name:      "payload encoded as a string",
			status:    http.StatusOK,
			response:  `[{"id":1,"payload":"{\"doorstep_deployment_id\":7}"},{"id":2,"payload":""},{"id":3,"payload":null},{"id":4},{"id":5,"payload":"not json"}]`,
			wantQuery: "per_page=100",
			want: []Deployment{
				{ID: 1, Payload: json.RawMessage(`{"doorstep_deployment_id":7}`)},
				{ID: 2},
				{ID: 3},
				{ID: 4},
				{ID: 5},
			},
		},
		{
			name:      "forbidden",
			status:    http.StatusForbidden,
			response:  "Resource not accessible by integration",
			wantQuery: "per_page=100",
			wantErr:   http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, rec := deploymentsFixture(t, "GET /repos/{owner}/{repo}/deployments", tt.status, tt.response)
			got, err := client.ListDeployments(t.Context(), "acme", "app", tt.filter)
			req := rec.only(t)
			if req.query != tt.wantQuery {
				t.Errorf("query = %q, want %q", req.query, tt.wantQuery)
			}
			if tt.wantErr != 0 {
				assertAPIStatus(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("ListDeployments: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d deployments, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				g, w := got[i], tt.want[i]
				if g.ID != w.ID || g.SHA != w.SHA || g.Ref != w.Ref || g.Task != w.Task || g.Environment != w.Environment || string(g.Payload) != string(w.Payload) {
					t.Errorf("deployment %d = %+v (payload %s), want %+v (payload %s)", i, g, g.Payload, w, w.Payload)
				}
			}
		})
	}
}

func assertAPIStatus(t *testing.T, err error, status int) {
	t.Helper()
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok || apiErr.StatusCode != status {
		t.Fatalf("error = %v, want *APIError with status %d", err, status)
	}
	if !strings.Contains(err.Error(), "acme/app") {
		t.Errorf("error %q does not name the repository", err)
	}
}

func TestCreateDeployment(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 200)
	tests := []struct {
		name     string
		req      DeploymentRequest
		status   int
		response string
		wantBody map[string]any
		want     Deployment
		wantErr  int
	}{
		{
			name: "full request",
			req: DeploymentRequest{
				Ref: testSHA, Environment: "blog", Task: "deploy", Description: "Deploy #42",
				Payload: map[string]any{"doorstep_deployment_id": 42, "kind": "deploy"},
			},
			status:   http.StatusCreated,
			response: `{"id":99,"sha":"` + testSHA + `","ref":"` + testSHA + `","environment":"blog","task":"deploy","payload":{"doorstep_deployment_id":42,"kind":"deploy"}}`,
			wantBody: map[string]any{
				"ref": testSHA, "environment": "blog", "task": "deploy", "description": "Deploy #42",
				"payload":           map[string]any{"doorstep_deployment_id": float64(42), "kind": "deploy"},
				"auto_merge":        false,
				"required_contexts": []any{},
			},
			want: Deployment{ID: 99, SHA: testSHA, Ref: testSHA, Environment: "blog", Task: "deploy"},
		},
		{
			name:     "minimal request still disables auto merge and required contexts",
			req:      DeploymentRequest{Ref: testSHA},
			status:   http.StatusCreated,
			response: `{"id":5}`,
			wantBody: map[string]any{"ref": testSHA, "auto_merge": false, "required_contexts": []any{}},
			want:     Deployment{ID: 5},
		},
		{
			name:     "long description is cut to 140 characters",
			req:      DeploymentRequest{Ref: testSHA, Description: long},
			status:   http.StatusCreated,
			response: `{"id":5}`,
			wantBody: map[string]any{
				"ref": testSHA, "description": strings.Repeat("é", 139) + "…",
				"auto_merge": false, "required_contexts": []any{},
			},
			want: Deployment{ID: 5},
		},
		{
			name:     "conflict",
			req:      DeploymentRequest{Ref: testSHA},
			status:   http.StatusConflict,
			response: "Conflict: Commit status checks failed for main.",
			wantErr:  http.StatusConflict,
		},
		{
			name:     "unprocessable",
			req:      DeploymentRequest{Ref: "nope"},
			status:   http.StatusUnprocessableEntity,
			response: "No ref found for: nope",
			wantErr:  http.StatusUnprocessableEntity,
		},
		{
			name:     "forbidden",
			req:      DeploymentRequest{Ref: testSHA},
			status:   http.StatusForbidden,
			response: "Resource not accessible by integration",
			wantErr:  http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, rec := deploymentsFixture(t, "POST /repos/{owner}/{repo}/deployments", tt.status, tt.response)
			got, err := client.CreateDeployment(t.Context(), "acme", "app", tt.req)
			req := rec.only(t)
			if tt.wantErr != 0 {
				assertAPIStatus(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("CreateDeployment: %v", err)
			}
			assertJSONEqual(t, req.body, tt.wantBody)
			if got.ID != tt.want.ID || got.SHA != tt.want.SHA || got.Environment != tt.want.Environment || got.Task != tt.want.Task {
				t.Errorf("CreateDeployment() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCreateDeploymentWithoutID(t *testing.T) {
	t.Parallel()

	client, _ := deploymentsFixture(t, "POST /repos/{owner}/{repo}/deployments", http.StatusAccepted, `{"message":"Auto-merged main into topic on deployment."}`)
	if _, err := client.CreateDeployment(t.Context(), "acme", "app", DeploymentRequest{Ref: testSHA}); err == nil || !strings.Contains(err.Error(), "no deployment id") {
		t.Fatalf("CreateDeployment() error = %v, want missing id", err)
	}
}

func TestCreateDeploymentStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		req      DeploymentStatusRequest
		status   int
		wantBody map[string]any
		wantErr  int
	}{
		{
			name:     "success with log url",
			req:      DeploymentStatusRequest{State: DeploymentSuccess, Description: "Deployed", LogURL: "https://doorstep.example.com/deployments/42", AutoInactive: true},
			status:   http.StatusCreated,
			wantBody: map[string]any{"state": "success", "description": "Deployed", "log_url": "https://doorstep.example.com/deployments/42", "auto_inactive": true},
		},
		{
			name:     "auto inactive false is sent",
			req:      DeploymentStatusRequest{State: DeploymentInactive},
			status:   http.StatusCreated,
			wantBody: map[string]any{"state": "inactive", "auto_inactive": false},
		},
		{
			name:    "not found",
			req:     DeploymentStatusRequest{State: DeploymentQueued},
			status:  http.StatusNotFound,
			wantErr: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, rec := deploymentsFixture(t, "POST /repos/{owner}/{repo}/deployments/{id}/statuses", tt.status, `{"id":1}`)
			err := client.CreateDeploymentStatus(t.Context(), "acme", "app", 99, tt.req)
			req := rec.only(t)
			if req.path != "/repos/acme/app/deployments/99/statuses" {
				t.Errorf("path = %q", req.path)
			}
			if tt.wantErr != 0 {
				assertAPIStatus(t, err, tt.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("CreateDeploymentStatus: %v", err)
			}
			assertJSONEqual(t, req.body, tt.wantBody)
		})
	}
}

func TestDeploymentMethodsRejectInvalidRepositories(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	client := f.repoClient(t, "acme", "app")
	tests := []struct {
		name string
		call func() error
	}{
		{name: "list", call: func() error {
			_, err := client.ListDeployments(t.Context(), "acme", "../x", DeploymentFilter{})
			return err
		}},
		{name: "create", call: func() error {
			_, err := client.CreateDeployment(t.Context(), "-acme", "app", DeploymentRequest{Ref: testSHA})
			return err
		}},
		{name: "status", call: func() error {
			return client.CreateDeploymentStatus(t.Context(), "acme", "a/b", 1, DeploymentStatusRequest{State: DeploymentSuccess})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.call(); !errors.Is(err, ErrInvalidRepository) {
				t.Errorf("error = %v, want ErrInvalidRepository", err)
			}
		})
	}
}

func TestTruncateDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "short", in: "Deployed", want: 8},
		{name: "exactly the limit", in: strings.Repeat("a", 140), want: 140},
		{name: "over the limit", in: strings.Repeat("a", 141), want: 140},
		{name: "multibyte", in: strings.Repeat("ü", 300), want: 140},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateDescription(tt.in)
			if n := utf8.RuneCountInString(got); n != tt.want {
				t.Errorf("truncateDescription() has %d characters, want %d", n, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateDescription() = %q is not valid UTF-8", got)
			}
		})
	}
}

func assertJSONEqual(t *testing.T, got, want map[string]any) {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(g) != string(w) {
		t.Errorf("body = %s, want %s", g, w)
	}
}
