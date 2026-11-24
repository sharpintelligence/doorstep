package gates_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

type fakeJobs struct {
	mu        sync.Mutex
	job       gates.Job
	err       error
	calls     int
	namespace string
	name      string
}

func (f *fakeJobs) Job(_ context.Context, namespace, name string) (gates.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.namespace, f.name = namespace, name
	return f.job, f.err
}

func (f *fakeJobs) set(job gates.Job, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.job, f.err = job, err
}

func (f *fakeJobs) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var (
	jobStarted   = time.Date(2026, 11, 24, 10, 0, 2, 0, time.UTC)
	jobCompleted = jobStarted.Add(95 * time.Second)
	postSync     = map[string]string{"argocd.argoproj.io/hook": "PostSync", "argocd.argoproj.io/hook-delete-policy": "BeforeHookCreation"}
)

func hookJob(mutate func(*gates.Job)) gates.Job {
	j := gates.Job{
		UID:          "0b6c7f4e-1a2b-4c3d-9e8f-001122334455",
		CreatedAt:    jobStarted.Add(-2 * time.Second),
		Annotations:  postSync,
		BackoffLimit: 6,
		StartedAt:    jobStarted,
	}
	if mutate != nil {
		mutate(&j)
	}
	return j
}

func jobConfig(logsURL string) config.Gate {
	return config.Gate{Type: config.GateJob, Name: "smoke", Job: &config.JobGate{Namespace: "web-prod", Name: "smoke-test", LogsURL: logsURL}}
}

func jobGate(t *testing.T, reader gates.JobReader, g config.Gate) gates.Gate {
	t.Helper()
	r := gates.NewRegistry()
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: reader}); err != nil {
		t.Fatalf("RegisterJobGate: %v", err)
	}
	app := webApp()
	app.Gates = config.Gates{Before: []config.Gate{}, After: []config.Gate{g}}
	plan, err := r.Plan(app)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan.After[0].Gate
}

func TestJobGateVerdicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		job         gates.Job
		err         error
		wantStatus  core.GateStatus
		wantMessage string
		wantFields  map[string]any
	}{
		{
			name: "complete passes with its runtime",
			job: hookJob(func(j *gates.Job) {
				j.Complete, j.Succeeded, j.CompletedAt = true, 1, jobCompleted
			}),
			wantStatus:  core.GatePassed,
			wantMessage: "Job web-prod/smoke-test succeeded in 1m35s",
			wantFields: map[string]any{
				"job": "web-prod/smoke-test", "found": true, "hook": true, "state": "succeeded",
				"uid": "0b6c7f4e-1a2b-4c3d-9e8f-001122334455", "created_at": "2026-11-24T10:00:00Z",
				"started_at": "2026-11-24T10:00:02Z", "completed_at": "2026-11-24T10:01:37Z",
				"succeeded": 1, "active": 0, "failed": 0, "backoff_limit": 6,
			},
		},
		{
			name:        "complete without timestamps leaves the runtime out",
			job:         hookJob(func(j *gates.Job) { j.Complete, j.StartedAt = true, time.Time{} }),
			wantStatus:  core.GatePassed,
			wantMessage: "Job web-prod/smoke-test succeeded",
			wantFields:  map[string]any{"started_at": nil, "completed_at": nil},
		},
		{
			name:        "completion before start leaves the runtime out",
			job:         hookJob(func(j *gates.Job) { j.Complete, j.CompletedAt = true, jobStarted.Add(-time.Minute) }),
			wantStatus:  core.GatePassed,
			wantMessage: "Job web-prod/smoke-test succeeded",
		},
		{
			name: "failed condition fails with reason and message",
			job: hookJob(func(j *gates.Job) {
				j.FailedCondition, j.Failed, j.BackoffLimit = true, 3, 2
				j.Reason, j.Message = "BackoffLimitExceeded", "Job has reached the specified backoff limit"
			}),
			wantStatus:  core.GateFailed,
			wantMessage: "Job web-prod/smoke-test failed: BackoffLimitExceeded: Job has reached the specified backoff limit",
			wantFields: map[string]any{
				"state": "failed", "failed": 3, "backoff_limit": 2,
				"reason": "BackoffLimitExceeded", "message": "Job has reached the specified backoff limit",
			},
		},
		{
			name:        "deadline exceeded fails",
			job:         hookJob(func(j *gates.Job) { j.FailedCondition, j.Reason = true, "DeadlineExceeded" }),
			wantStatus:  core.GateFailed,
			wantMessage: "Job web-prod/smoke-test failed: DeadlineExceeded",
		},
		{
			name:        "failed condition without detail",
			job:         hookJob(func(j *gates.Job) { j.FailedCondition = true }),
			wantStatus:  core.GateFailed,
			wantMessage: "Job web-prod/smoke-test failed",
		},
		{
			name:        "running is pending with progress",
			job:         hookJob(func(j *gates.Job) { j.Active, j.Failed = 1, 2 }),
			wantStatus:  core.GatePending,
			wantMessage: "Job web-prod/smoke-test is running: 1 active, 2 failed of backoff limit 6",
			wantFields:  map[string]any{"state": "active", "active": 1, "failed": 2},
		},
		{
			name:        "created but not started is pending",
			job:         hookJob(func(j *gates.Job) { j.StartedAt = time.Time{} }),
			wantStatus:  core.GatePending,
			wantMessage: "is running: 0 active, 0 failed of backoff limit 6",
		},
		{
			name:        "pod failures within the backoff limit stay pending",
			job:         hookJob(func(j *gates.Job) { j.Failed = 5 }),
			wantStatus:  core.GatePending,
			wantMessage: "5 failed of backoff limit 6",
		},
		{
			name:        "suspended is pending",
			job:         hookJob(func(j *gates.Job) { j.Suspended = true }),
			wantStatus:  core.GatePending,
			wantMessage: "Job web-prod/smoke-test is suspended",
			wantFields:  map[string]any{"state": "suspended"},
		},
		{
			name:        "being deleted is pending",
			job:         hookJob(func(j *gates.Job) { j.Deleting, j.Complete = true, true }),
			wantStatus:  core.GatePending,
			wantMessage: "Job web-prod/smoke-test is being replaced; waiting for Argo CD to recreate it",
			wantFields:  map[string]any{"state": "terminating"},
		},
		{
			name:        "not found is pending",
			err:         fmt.Errorf("get: %w", gates.ErrJobNotFound),
			wantStatus:  core.GatePending,
			wantMessage: "Job web-prod/smoke-test not found; waiting for Argo CD to create it",
			wantFields: map[string]any{
				"job": "web-prod/smoke-test", "found": false, "hook": false, "state": "not_found", "uid": nil, "created_at": nil,
			},
		},
		{
			name:        "a succeeded Job that is not a hook fails",
			job:         hookJob(func(j *gates.Job) { j.Annotations, j.Complete = nil, true }),
			wantStatus:  core.GateFailed,
			wantMessage: "Job web-prod/smoke-test is not an Argo CD PostSync hook, so it does not rerun on each deploy",
			wantFields:  map[string]any{"state": "not_a_hook", "hook": false},
		},
		{
			name:        "a running Job that is not a hook fails",
			job:         hookJob(func(j *gates.Job) { j.Annotations, j.Active = map[string]string{"app": "smoke"}, 1 }),
			wantStatus:  core.GateFailed,
			wantMessage: "is not an Argo CD PostSync hook",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeJobs{}
			reader.set(tt.job, tt.err)
			g := jobGate(t, reader, jobConfig(""))
			ev := evaluateGate(t, g, deployment(core.KindDeploy), nil)
			if ev.Status != tt.wantStatus || !strings.Contains(ev.Message, tt.wantMessage) {
				t.Fatalf("Evaluate = %s %q, want %s %q", ev.Status, ev.Message, tt.wantStatus, tt.wantMessage)
			}
			evidence := evidenceMap(t, ev)
			for k, want := range tt.wantFields {
				if got := evidence[k]; fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("evidence %s = %v, want %v (%s)", k, got, want, ev.Evidence)
				}
			}
			if _, ok := evidence["logs_url"]; ok {
				t.Errorf("evidence has logs_url without a template: %s", ev.Evidence)
			}
			if reader.namespace != "web-prod" || reader.name != "smoke-test" {
				t.Errorf("read Job %s/%s, want web-prod/smoke-test", reader.namespace, reader.name)
			}
		})
	}
}

func TestJobGateHookAnnotations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		annotations map[string]string
		want        core.GateStatus
	}{
		{name: "PostSync", annotations: map[string]string{"argocd.argoproj.io/hook": "PostSync"}, want: core.GatePassed},
		{name: "PostSync among several hooks", annotations: map[string]string{"argocd.argoproj.io/hook": "PostSync,SyncFail"}, want: core.GatePassed},
		{name: "spaces around hooks", annotations: map[string]string{"argocd.argoproj.io/hook": "SyncFail , PostSync "}, want: core.GatePassed},
		{name: "helm post-upgrade", annotations: map[string]string{"helm.sh/hook": "post-upgrade"}, want: core.GatePassed},
		{name: "helm post-install among several", annotations: map[string]string{"helm.sh/hook": "pre-install,post-install"}, want: core.GatePassed},
		{name: "empty Argo CD hook falls back to helm", annotations: map[string]string{"argocd.argoproj.io/hook": " ", "helm.sh/hook": "post-upgrade"}, want: core.GatePassed},
		{name: "Argo CD hook takes precedence over helm", annotations: map[string]string{"argocd.argoproj.io/hook": "PreSync", "helm.sh/hook": "post-upgrade"}, want: core.GateFailed},
		{name: "PreSync only", annotations: map[string]string{"argocd.argoproj.io/hook": "PreSync"}, want: core.GateFailed},
		{name: "Sync hook", annotations: map[string]string{"argocd.argoproj.io/hook": "Sync"}, want: core.GateFailed},
		{name: "wrong case", annotations: map[string]string{"argocd.argoproj.io/hook": "postsync"}, want: core.GateFailed},
		{name: "partial token", annotations: map[string]string{"argocd.argoproj.io/hook": "PostSyncX"}, want: core.GateFailed},
		{name: "helm pre-upgrade", annotations: map[string]string{"helm.sh/hook": "pre-upgrade"}, want: core.GateFailed},
		{name: "helm test", annotations: map[string]string{"helm.sh/hook": "test"}, want: core.GateFailed},
		{name: "no annotations", annotations: nil, want: core.GateFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeJobs{}
			reader.set(hookJob(func(j *gates.Job) { j.Annotations, j.Complete = tt.annotations, true }), nil)
			ev := evaluateGate(t, jobGate(t, reader, jobConfig("")), deployment(core.KindDeploy), nil)
			if ev.Status != tt.want {
				t.Errorf("Evaluate = %s %q, want %s", ev.Status, ev.Message, tt.want)
			}
		})
	}
}

func TestJobGateLogsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		job      gates.Job
		err      error
		want     string
	}{
		{
			name:     "every placeholder",
			template: "https://logs.example.com/{app}/{namespace}/{job}/{uid}/{deployment}/{argocd_namespace}/{argocd_application}",
			job:      hookJob(func(j *gates.Job) { j.Complete = true }),
			want:     "https://logs.example.com/web/web-prod/smoke-test/0b6c7f4e-1a2b-4c3d-9e8f-001122334455/7/argocd/web-prod",
		},
		{
			name:     "argo cd ui",
			template: "https://argocd.example.com/applications/{argocd_namespace}/{argocd_application}?node=batch%2FJob%2F{namespace}%2F{job}%2F0&tab=logs",
			job:      hookJob(func(j *gates.Job) { j.Active = 1 }),
			want:     "https://argocd.example.com/applications/argocd/web-prod?node=batch%2FJob%2Fweb-prod%2Fsmoke-test%2F0&tab=logs",
		},
		{
			name:     "uid from the cluster is escaped",
			template: "https://logs.example.com/{uid}",
			job:      hookJob(func(j *gates.Job) { j.UID = "a/b?c#d" }),
			want:     "https://logs.example.com/a%2Fb%3Fc%23d",
		},
		{
			name:     "not found renders with an empty uid",
			template: "https://logs.example.com/{job}/{uid}",
			err:      gates.ErrJobNotFound,
			want:     "https://logs.example.com/smoke-test/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeJobs{}
			reader.set(tt.job, tt.err)
			ev := evaluateGate(t, jobGate(t, reader, jobConfig(tt.template)), deployment(core.KindDeploy), nil)
			if got := evidenceMap(t, ev)["logs_url"]; got != tt.want {
				t.Errorf("logs_url = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestJobGateReadErrors(t *testing.T) {
	t.Parallel()

	forbidden := errors.New("kube api: 403 Forbidden: jobs.batch is forbidden")
	tests := []struct {
		name string
		err  error
	}{
		{name: "forbidden", err: forbidden},
		{name: "wrapped", err: fmt.Errorf("job web-prod/smoke-test: %w", forbidden)},
		{name: "deadline", err: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeJobs{}
			reader.set(gates.Job{}, tt.err)
			g := jobGate(t, reader, jobConfig("https://logs.example.com/{job}"))
			_, err := g.Evaluate(t.Context(), gates.Request{Deployment: deployment(core.KindDeploy), Phase: core.GatePhaseAfter})
			if !errors.Is(err, tt.err) || !strings.Contains(err.Error(), "web-prod/smoke-test") {
				t.Errorf("Evaluate error = %v, want %v naming the Job", err, tt.err)
			}
		})
	}
}

func TestJobGateCleansClusterStrings(t *testing.T) {
	t.Parallel()

	reader := &fakeJobs{}
	hostile := "bad\x00\xff" + strings.Repeat("m", 5000)
	reader.set(hookJob(func(j *gates.Job) {
		j.FailedCondition, j.Reason, j.Message, j.UID = true, hostile, hostile, hostile
	}), nil)
	ev := evaluateGate(t, jobGate(t, reader, jobConfig("")), deployment(core.KindDeploy), nil)
	evidence := evidenceMap(t, ev)
	for _, key := range []string{"reason", "message", "uid"} {
		s, _ := evidence[key].(string)
		if strings.Contains(s, "\x00") || len(s) > core.MaxGateMessageBytes || !strings.HasPrefix(s, "bad") {
			t.Errorf("evidence %s not cleaned: %d bytes", key, len(s))
		}
	}
}

func TestJobGateIsStateless(t *testing.T) {
	t.Parallel()

	reader := &fakeJobs{}
	reader.set(hookJob(func(j *gates.Job) { j.Active = 1 }), nil)
	g := jobGate(t, reader, jobConfig(""))
	first := evaluateGate(t, g, deployment(core.KindDeploy), nil)
	reader.set(gates.Job{}, gates.ErrJobNotFound)
	second := evaluateGate(t, g, deployment(core.KindDeploy), resultOf(first))
	if got := evidenceMap(t, second); got["found"] != false || got["active"] != float64(0) {
		t.Errorf("evidence carried from the previous result: %s", second.Evidence)
	}
}

func TestRegisterJobGate(t *testing.T) {
	t.Parallel()

	if err := gates.RegisterJobGate(nil, gates.JobGateDeps{Jobs: &fakeJobs{}}); err == nil || !strings.Contains(err.Error(), "no registry") {
		t.Errorf("RegisterJobGate(nil) = %v", err)
	}
	if err := gates.RegisterJobGate(gates.NewRegistry(), gates.JobGateDeps{}); err == nil || !strings.Contains(err.Error(), "needs a job reader") {
		t.Errorf("RegisterJobGate without a reader = %v", err)
	}
	r := gates.NewRegistry()
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: &fakeJobs{}}); err != nil {
		t.Fatalf("RegisterJobGate: %v", err)
	}
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: &fakeJobs{}}); !errors.Is(err, gates.ErrDuplicateGateType) {
		t.Errorf("second RegisterJobGate = %v, want ErrDuplicateGateType", err)
	}
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
		t.Errorf("RegisterBuiltins after RegisterJobGate: %v", err)
	}
}

func TestJobGateFactoryNeedsSettings(t *testing.T) {
	t.Parallel()

	r := gates.NewRegistry()
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: &fakeJobs{}}); err != nil {
		t.Fatalf("RegisterJobGate: %v", err)
	}
	app := webApp()
	app.Gates = config.Gates{After: []config.Gate{{Type: config.GateJob, Name: "smoke"}}}
	if _, err := r.Plan(app); err == nil || !strings.Contains(err.Error(), "job gate has no job settings") {
		t.Errorf("Plan without job settings = %v", err)
	}
}

const jobGateConfig = `
apps:
  - name: web
    source: {repo: acme/web}
    image: {repository: ghcr.io/acme/web}
    gitops: {repo: acme/platform, path: apps/web}
    argocd: {application: web-prod}
    gates:
      after:
        - type: health
        - type: job
          name: smoke
          job: {namespace: web-prod, name: smoke-test}
`

func jobPlan(t *testing.T, reader gates.JobReader, f *builtinFakes) gates.Plan {
	t.Helper()
	catalog := loadCatalog(t, jobGateConfig)
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, f.deps()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: reader}); err != nil {
		t.Fatalf("RegisterJobGate: %v", err)
	}
	plans, err := r.Plans(catalog)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	return plans["web"]
}

func TestJobGatePlan(t *testing.T) {
	t.Parallel()

	plan := jobPlan(t, &fakeJobs{}, newBuiltinFakes())
	if len(plan.After) != 2 {
		t.Fatalf("plan has %d after gates, want 2", len(plan.After))
	}
	b := plan.After[1]
	if b.Name != "smoke" || b.Type != config.GateJob || b.Kind != metrics.GateKindJob || !b.SkipOnRollback ||
		b.Timeout != 10*time.Minute || b.Phase != core.GatePhaseAfter || b.Gate == nil {
		t.Errorf("job binding = %+v", b)
	}
}

func TestJobGateSkippedOnRollback(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.apps.set(healthyApp, nil)
	reader := &fakeJobs{}
	reader.set(hookJob(func(j *gates.Job) { j.FailedCondition = true }), nil)
	h := newHarness(t, jobPlan(t, reader, f), func(o *gates.Options) { o.Now = f.clock.Now })

	out := mustEvaluate(t, h, deployment(core.KindRollback), core.GatePhaseAfter)
	if got, want := strings.Join(outcomeStatuses(out), " "), "health=passed smoke=skipped"; got != want || out.Status != core.GatePassed {
		t.Errorf("rollback after gates = %s (%s), want %s and passed", got, out.Status, want)
	}
	if out.Results[1].Message != "skipped for rollback" {
		t.Errorf("job gate message = %q, want skipped for rollback", out.Results[1].Message)
	}
	if n := reader.callCount(); n != 0 {
		t.Errorf("job gate read the Job %d times for a rollback", n)
	}
}

func TestJobGateTimesOutWhilePending(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.apps.set(healthyApp, nil)
	reader := &fakeJobs{}
	reader.set(hookJob(func(j *gates.Job) { j.Active = 1 }), nil)
	h := newHarness(t, jobPlan(t, reader, f))
	d := deployment(core.KindDeploy)

	out := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if got, want := strings.Join(outcomeStatuses(out), " "), "health=passed smoke=pending"; got != want {
		t.Fatalf("first evaluation = %s, want %s", got, want)
	}
	h.clock.Advance(9 * time.Minute)
	if out = mustEvaluate(t, h, d, core.GatePhaseAfter); out.Status != core.GatePending {
		t.Fatalf("after 9m = %s, want pending", out.Status)
	}
	reader.set(gates.Job{}, gates.ErrJobNotFound)
	h.clock.Advance(time.Minute)
	out = mustEvaluate(t, h, d, core.GatePhaseAfter)
	if out.Status != core.GateFailed || out.Results[1].Message != "timed out after 10m0s" {
		t.Errorf("after 10m = %s %q, want failed by the timeout", out.Status, out.Results[1].Message)
	}

	reader.set(hookJob(func(j *gates.Job) { j.Complete = true }), nil)
	h.clock.Advance(time.Minute)
	if out = mustEvaluate(t, h, d, core.GatePhaseAfter); out.Status != core.GateFailed {
		t.Errorf("a later success changed the sticky failure to %s", out.Status)
	}
}

func TestJobGateEvaluationErrorStaysPending(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.apps.set(healthyApp, nil)
	reader := &fakeJobs{}
	reader.set(gates.Job{}, errors.New("kube api: 403 Forbidden"))
	h := newHarness(t, jobPlan(t, reader, f))

	out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseAfter)
	res := out.Results[1]
	if res.Status != core.GatePending || !strings.Contains(res.Message, "evaluation error: read Job web-prod/smoke-test: kube api: 403 Forbidden") {
		t.Errorf("job gate = %s %q, want pending with the evaluation error", res.Status, res.Message)
	}
}
