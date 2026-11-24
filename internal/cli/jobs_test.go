package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/jobs"
	"github.com/sharpintelligence/doorstep/internal/kube"
)

var _ gates.JobReader = jobReader{}

func TestGatesJob(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	base := jobs.Job{
		Namespace: "web", Name: "smoke", UID: "u-1", CreatedAt: started.Add(-time.Second),
		Annotations: map[string]string{"argocd.argoproj.io/hook": "PostSync"}, BackoffLimit: 3,
		Active: 1, Succeeded: 2, Failed: 1, StartedAt: started, CompletedAt: started.Add(time.Minute),
	}
	baseWant := gates.Job{
		UID: "u-1", CreatedAt: started.Add(-time.Second),
		Annotations: map[string]string{"argocd.argoproj.io/hook": "PostSync"}, BackoffLimit: 3,
		Active: 1, Succeeded: 2, Failed: 1, StartedAt: started, CompletedAt: started.Add(time.Minute),
	}
	tests := []struct {
		name   string
		mutate func(*jobs.Job)
		want   func(*gates.Job)
	}{
		{name: "no conditions", mutate: func(*jobs.Job) {}, want: func(*gates.Job) {}},
		{
			name: "complete",
			mutate: func(j *jobs.Job) {
				j.Conditions = []jobs.Condition{
					{Type: "SuccessCriteriaMet", Status: "True", Reason: "CompletionsReached"},
					{Type: "Complete", Status: "True", Reason: "CompletionsReached", Message: "done"},
				}
			},
			want: func(j *gates.Job) { j.Complete = true },
		},
		{
			name: "failed",
			mutate: func(j *jobs.Job) {
				j.Conditions = []jobs.Condition{
					{Type: "FailureTarget", Status: "True", Reason: "BackoffLimitExceeded", Message: "target"},
					{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"},
				}
			},
			want: func(j *gates.Job) {
				j.FailedCondition, j.Reason, j.Message = true, "BackoffLimitExceeded", "Job has reached the specified backoff limit"
			},
		},
		{
			name: "conditions that are not true are ignored",
			mutate: func(j *jobs.Job) {
				j.Conditions = []jobs.Condition{
					{Type: "Complete", Status: "False"},
					{Type: "Failed", Status: "Unknown", Reason: "Maybe"},
					{Type: "Suspended", Status: "True"},
				}
			},
			want: func(*gates.Job) {},
		},
		{
			name:   "flags are copied",
			mutate: func(j *jobs.Job) { j.Deleting, j.Suspended = true, true },
			want:   func(j *gates.Job) { j.Deleting, j.Suspended = true, true },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := base
			tt.mutate(&in)
			want := baseWant
			tt.want(&want)
			if got := gatesJob(in); !reflect.DeepEqual(got, want) {
				t.Errorf("gatesJob =\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

type jobsAPI struct {
	status int
	body   any
}

func (a jobsAPI) serve(t *testing.T) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/batch/v1/namespaces/web-prod/jobs/smoke-test" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		_ = json.NewEncoder(w).Encode(a.body)
	}))
	t.Cleanup(srv.Close)
	client, err := kube.New(kube.Config{Server: srv.URL}, kube.Options{})
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	return client
}

func jobObject(annotations map[string]string, status map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name": "smoke-test", "namespace": "web-prod", "uid": "9d1c",
			"creationTimestamp": "2026-11-24T10:00:00Z", "annotations": annotations,
		},
		"spec":   map[string]any{"backoffLimit": 2},
		"status": status,
	}
}

const jobGateServeConfig = `
apps:
  - name: web
    source: {repo: acme/web}
    image: {repository: ghcr.io/acme/web}
    gitops: {repo: acme/platform, path: apps/web}
    argocd: {application: web-prod}
    gates:
      before: []
      after:
        - type: job
          name: smoke
          job:
            namespace: web-prod
            name: smoke-test
            logs_url: https://argocd.example.com/applications/{argocd_namespace}/{argocd_application}?node=batch%2FJob%2F{namespace}%2F{job}%2F0&tab=logs
`

func TestRegisterJobGateEndToEnd(t *testing.T) {
	t.Parallel()

	hook := map[string]string{"argocd.argoproj.io/hook": "PostSync"}
	tests := []struct {
		name        string
		api         jobsAPI
		wantStatus  core.GateStatus
		wantMessage string
		wantErr     error
	}{
		{
			name: "succeeded",
			api: jobsAPI{body: jobObject(hook, map[string]any{
				"succeeded": 1, "startTime": "2026-11-24T10:00:01Z", "completionTime": "2026-11-24T10:00:31Z",
				"conditions": []map[string]any{{"type": "Complete", "status": "True"}},
			})},
			wantStatus:  core.GatePassed,
			wantMessage: "Job web-prod/smoke-test succeeded in 30s",
		},
		{
			name: "failed",
			api: jobsAPI{body: jobObject(hook, map[string]any{
				"failed": 3,
				"conditions": []map[string]any{{
					"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded", "message": "Job has reached the specified backoff limit",
				}},
			})},
			wantStatus:  core.GateFailed,
			wantMessage: "Job web-prod/smoke-test failed: BackoffLimitExceeded: Job has reached the specified backoff limit",
		},
		{
			name:        "running",
			api:         jobsAPI{body: jobObject(hook, map[string]any{"active": 1, "failed": 1})},
			wantStatus:  core.GatePending,
			wantMessage: "Job web-prod/smoke-test is running: 1 active, 1 failed of backoff limit 2",
		},
		{
			name:        "not a hook",
			api:         jobsAPI{body: jobObject(nil, map[string]any{"succeeded": 1, "conditions": []map[string]any{{"type": "Complete", "status": "True"}}})},
			wantStatus:  core.GateFailed,
			wantMessage: "is not an Argo CD PostSync hook",
		},
		{
			name:        "not found",
			api:         jobsAPI{status: http.StatusNotFound, body: map[string]any{"kind": "Status", "code": 404, "reason": "NotFound"}},
			wantStatus:  core.GatePending,
			wantMessage: "Job web-prod/smoke-test not found; waiting for Argo CD to create it",
		},
		{
			name:    "forbidden",
			api:     jobsAPI{status: http.StatusForbidden, body: map[string]any{"kind": "Status", "code": 403, "message": "jobs.batch is forbidden"}},
			wantErr: kube.ErrForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, err := config.Parse(strings.NewReader(jobGateServeConfig))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			catalog := f.Catalog()
			if err := catalog.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			registry := gates.NewRegistry()
			if err := registerJobGate(registry, tt.api.serve(t)); err != nil {
				t.Fatalf("registerJobGate: %v", err)
			}
			plan, err := registry.Plan(catalog.Apps[0])
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			d := core.Deployment{ID: 42, Target: core.AppTarget("web"), Kind: core.KindDeploy}
			ev, err := plan.After[0].Gate.Evaluate(t.Context(), gates.Request{Deployment: d, Phase: core.GatePhaseAfter})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Evaluate error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if ev.Status != tt.wantStatus || !strings.Contains(ev.Message, tt.wantMessage) {
				t.Errorf("Evaluate = %s %q, want %s %q", ev.Status, ev.Message, tt.wantStatus, tt.wantMessage)
			}
			var evidence map[string]any
			if err := json.Unmarshal(ev.Evidence, &evidence); err != nil {
				t.Fatalf("decode evidence: %v", err)
			}
			want := "https://argocd.example.com/applications/argocd/web-prod?node=batch%2FJob%2Fweb-prod%2Fsmoke-test%2F0&tab=logs"
			if evidence["logs_url"] != want {
				t.Errorf("logs_url = %v, want %s", evidence["logs_url"], want)
			}
		})
	}
}

func TestRegisterJobGateErrors(t *testing.T) {
	t.Parallel()

	if err := registerJobGate(gates.NewRegistry(), nil); err == nil {
		t.Error("registerJobGate accepted a nil client")
	}
	client := jobsAPI{}.serve(t)
	if err := registerJobGate(nil, client); err == nil {
		t.Error("registerJobGate accepted a nil registry")
	}
	registry := gates.NewRegistry()
	if err := registerJobGate(registry, client); err != nil {
		t.Fatalf("registerJobGate: %v", err)
	}
	if err := registerJobGate(registry, client); !errors.Is(err, gates.ErrDuplicateGateType) {
		t.Errorf("second registerJobGate = %v, want ErrDuplicateGateType", err)
	}
}

func TestJobReaderMapsNotFound(t *testing.T) {
	t.Parallel()

	reader, err := jobs.New(jobsAPI{status: http.StatusNotFound, body: map[string]any{"code": 404}}.serve(t))
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	_, err = jobReader{reader: reader}.Job(t.Context(), "web-prod", "smoke-test")
	if !errors.Is(err, gates.ErrJobNotFound) || !errors.Is(err, jobs.ErrNotFound) {
		t.Errorf("Job error = %v, want gates.ErrJobNotFound wrapping jobs.ErrNotFound", err)
	}
}
