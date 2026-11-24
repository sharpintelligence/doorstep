package jobs_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/jobs"
	"github.com/sharpintelligence/doorstep/internal/kube"
)

var _ jobs.Getter = (*kube.Client)(nil)

type fakeAPI struct {
	mu     sync.Mutex
	status int
	body   string
	paths  []string
	raw    []string
	block  chan struct{}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.raw = append(f.raw, r.URL.EscapedPath())
	status, body, block := f.status, f.body, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeAPI) requests() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.raw...)
}

func newReader(t *testing.T, api *fakeAPI) *jobs.Reader {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	client, err := kube.New(kube.Config{Server: srv.URL}, kube.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	reader, err := jobs.New(client)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	return reader
}

func TestReaderJob(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	started := created.Add(2 * time.Second)
	completed := started.Add(95 * time.Second)
	tests := []struct {
		name string
		body string
		want jobs.Job
	}{
		{
			name: "complete",
			body: `{
				"metadata": {
					"name": "smoke", "namespace": "web", "uid": "u-1", "creationTimestamp": "2026-11-24T10:00:00Z",
					"annotations": {"argocd.argoproj.io/hook": "PostSync", "argocd.argoproj.io/hook-delete-policy": "BeforeHookCreation"}
				},
				"spec": {"backoffLimit": 2, "suspend": false, "template": {"spec": {"containers": [{"name": "t", "env": [{"name": "TOKEN", "value": "secret"}]}]}}},
				"status": {
					"succeeded": 1, "startTime": "2026-11-24T10:00:02Z", "completionTime": "2026-11-24T10:01:37Z",
					"conditions": [
						{"type": "SuccessCriteriaMet", "status": "True", "reason": "CompletionsReached", "message": "Reached expected number of succeeded pods"},
						{"type": "Complete", "status": "True", "reason": "CompletionsReached", "message": "Reached expected number of succeeded pods"}
					]
				}
			}`,
			want: jobs.Job{
				UID: "u-1", CreatedAt: created,
				Annotations:  map[string]string{"argocd.argoproj.io/hook": "PostSync", "argocd.argoproj.io/hook-delete-policy": "BeforeHookCreation"},
				BackoffLimit: 2, Succeeded: 1, StartedAt: started, CompletedAt: completed,
				Conditions: []jobs.Condition{
					{Type: "SuccessCriteriaMet", Status: "True", Reason: "CompletionsReached", Message: "Reached expected number of succeeded pods"},
					{Type: "Complete", Status: "True", Reason: "CompletionsReached", Message: "Reached expected number of succeeded pods"},
				},
			},
		},
		{
			name: "failed with backoff limit exceeded",
			body: `{
				"metadata": {"name": "smoke", "namespace": "web", "uid": "u-2"},
				"spec": {"backoffLimit": 1},
				"status": {"failed": 2, "startTime": "2026-11-24T10:00:02Z",
					"conditions": [{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded", "message": "Job has reached the specified backoff limit"}]}
			}`,
			want: jobs.Job{
				UID: "u-2", BackoffLimit: 1, Failed: 2, StartedAt: started,
				Conditions: []jobs.Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}},
			},
		},
		{
			name: "active",
			body: `{"metadata": {"name": "smoke", "namespace": "web"}, "spec": {"backoffLimit": 6}, "status": {"active": 1, "failed": 2}}`,
			want: jobs.Job{BackoffLimit: 6, Active: 1, Failed: 2},
		},
		{
			name: "suspended",
			body: `{"metadata": {"name": "smoke", "namespace": "web"}, "spec": {"suspend": true, "backoffLimit": 0}}`,
			want: jobs.Job{Suspended: true},
		},
		{
			name: "deleting",
			body: `{"metadata": {"name": "smoke", "namespace": "web", "deletionTimestamp": "2026-11-24T10:05:00Z"}, "spec": {}}`,
			want: jobs.Job{Deleting: true, BackoffLimit: jobs.DefaultBackoffLimit},
		},
		{
			name: "unset backoff limit defaults to six",
			body: `{"metadata": {"name": "smoke", "namespace": "web"}}`,
			want: jobs.Job{BackoffLimit: 6},
		},
		{
			name: "unparsable timestamps are left zero",
			body: `{"metadata": {"name": "smoke", "namespace": "web", "creationTimestamp": "yesterday"}, "status": {"startTime": "soon", "completionTime": "2026-13-45T99:00:00Z"}}`,
			want: jobs.Job{BackoffLimit: 6},
		},
		{
			name: "offset timestamps are normalised to UTC",
			body: `{"metadata": {"name": "smoke", "namespace": "web", "creationTimestamp": "2026-11-24T11:00:00+01:00"}}`,
			want: jobs.Job{BackoffLimit: 6, CreatedAt: created},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := &fakeAPI{body: tt.body}
			got, err := newReader(t, api).Job(t.Context(), "web", "smoke")
			if err != nil {
				t.Fatalf("Job: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Job =\n%+v\nwant\n%+v", got, tt.want)
			}
			paths, _ := api.requests()
			if len(paths) != 1 || paths[0] != "/apis/batch/v1/namespaces/web/jobs/smoke" {
				t.Errorf("requests = %v, want one GET of the Job", paths)
			}
		})
	}
}

func TestReaderJobErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		status       int
		body         string
		wantNotFound bool
		wantIs       error
		wantText     string
	}{
		{
			name: "not found", status: http.StatusNotFound,
			body:         `{"kind":"Status","code":404,"reason":"NotFound","message":"jobs.batch \"smoke\" not found"}`,
			wantNotFound: true, wantIs: kube.ErrNotFound, wantText: "job web/smoke",
		},
		{
			name: "forbidden", status: http.StatusForbidden,
			body:   `{"kind":"Status","code":403,"reason":"Forbidden","message":"jobs.batch \"smoke\" is forbidden"}`,
			wantIs: kube.ErrForbidden, wantText: "is forbidden",
		},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{}`, wantIs: kube.ErrForbidden},
		{name: "server error", status: http.StatusInternalServerError, body: `{}`, wantText: "500"},
		{name: "malformed body", status: http.StatusOK, body: `{"metadata":`, wantText: "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := newReader(t, &fakeAPI{status: tt.status, body: tt.body}).Job(t.Context(), "web", "smoke")
			if err == nil {
				t.Fatal("Job returned no error")
			}
			if got := errors.Is(err, jobs.ErrNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(%v, ErrNotFound) = %v, want %v", err, got, tt.wantNotFound)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not contain %q", err, tt.wantText)
			}
		})
	}
}

func TestReaderJobEscapesPath(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{body: `{}`}
	if _, err := newReader(t, api).Job(t.Context(), "team a", "smoke?x=1#y"); err != nil {
		t.Fatalf("Job: %v", err)
	}
	paths, raw := api.requests()
	if len(paths) != 1 || paths[0] != "/apis/batch/v1/namespaces/team a/jobs/smoke?x=1#y" {
		t.Errorf("paths = %v", paths)
	}
	if raw[0] != "/apis/batch/v1/namespaces/team%20a/jobs/smoke%3Fx=1%23y" {
		t.Errorf("escaped path = %q", raw[0])
	}
}

func TestReaderJobRejectsInvalidReferences(t *testing.T) {
	t.Parallel()

	for _, ref := range [][2]string{{"", "smoke"}, {"web", ""}, {"..", "smoke"}, {"web", ".."}, {"web", "."}} {
		api := &fakeAPI{body: `{}`}
		if _, err := newReader(t, api).Job(t.Context(), ref[0], ref[1]); err == nil {
			t.Errorf("Job(%q, %q) accepted an invalid reference", ref[0], ref[1])
		}
		if paths, _ := api.requests(); len(paths) != 0 {
			t.Errorf("Job(%q, %q) sent %v", ref[0], ref[1], paths)
		}
	}
}

func TestReaderJobHonoursContext(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	defer close(block)
	reader := newReader(t, &fakeAPI{body: `{}`, block: block})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := reader.Job(ctx, "web", "smoke")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Job error = %v, want context.DeadlineExceeded", err)
	}
}

func TestNewRequiresClient(t *testing.T) {
	t.Parallel()

	if _, err := jobs.New(nil); err == nil {
		t.Error("New accepted a nil client")
	}
}
