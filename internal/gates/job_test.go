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
	syncStarted  = jobStarted.Add(-10 * time.Second)
	syncedApp    = gates.Application{
		SyncStatus: gates.SyncSynced, Health: gates.HealthHealthy, Revision: "abc123",
		OperationPhase: gates.OperationSucceeded, OperationStartedAt: syncStarted,
	}
)

func syncedApps() *fakeApplications {
	apps := &fakeApplications{}
	apps.set(syncedApp, nil)
	return apps
}

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
	return jobGateWith(t, reader, syncedApps(), g)
}

func jobGateWith(t *testing.T, reader gates.JobReader, apps gates.ApplicationReader, g config.Gate) gates.Gate {
	t.Helper()
	r := gates.NewRegistry()
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: reader, Applications: apps}); err != nil {
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
				"application": "argocd/web-prod", "operation_phase": "Succeeded", "operation_started_at": "2026-11-24T09:59:52Z",
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
			name:     "query values are query escaped",
			template: "https://logs.example.com/search?uid={uid}",
			job:      hookJob(func(j *gates.Job) { j.UID = "a&b=c+d" }),
			want:     "https://logs.example.com/search?uid=a%26b%3Dc%2Bd",
		},
		{
			name:     "not found leaves out a link that needs the uid",
			template: "https://logs.example.com/{job}/{uid}",
			err:      gates.ErrJobNotFound,
			want:     "",
		},
		{
			name:     "not found keeps a link without the uid",
			template: "https://logs.example.com/{job}",
			err:      gates.ErrJobNotFound,
			want:     "https://logs.example.com/smoke-test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeJobs{}
			reader.set(tt.job, tt.err)
			ev := evaluateGate(t, jobGate(t, reader, jobConfig(tt.template)), deployment(core.KindDeploy), nil)
			got, ok := evidenceMap(t, ev)["logs_url"]
			if tt.want == "" && ok {
				t.Errorf("logs_url = %v, want none", got)
			}
			if tt.want != "" && got != tt.want {
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

	if err := gates.RegisterJobGate(nil, gates.JobGateDeps{Jobs: &fakeJobs{}, Applications: &fakeApplications{}}); err == nil || !strings.Contains(err.Error(), "no registry") {
		t.Errorf("RegisterJobGate(nil) = %v", err)
	}
	if err := gates.RegisterJobGate(gates.NewRegistry(), gates.JobGateDeps{Applications: &fakeApplications{}}); err == nil || !strings.Contains(err.Error(), "needs a job reader") {
		t.Errorf("RegisterJobGate without a reader = %v", err)
	}
	if err := gates.RegisterJobGate(gates.NewRegistry(), gates.JobGateDeps{Jobs: &fakeJobs{}}); err == nil || !strings.Contains(err.Error(), "needs an Argo CD application reader") {
		t.Errorf("RegisterJobGate without an application reader = %v", err)
	}
	r := gates.NewRegistry()
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: &fakeJobs{}, Applications: &fakeApplications{}}); err != nil {
		t.Fatalf("RegisterJobGate: %v", err)
	}
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: &fakeJobs{}, Applications: &fakeApplications{}}); !errors.Is(err, gates.ErrDuplicateGateType) {
		t.Errorf("second RegisterJobGate = %v, want ErrDuplicateGateType", err)
	}
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
		t.Errorf("RegisterBuiltins after RegisterJobGate: %v", err)
	}
}

func TestJobGateFactoryNeedsSettings(t *testing.T) {
	t.Parallel()

	r := gates.NewRegistry()
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: &fakeJobs{}, Applications: &fakeApplications{}}); err != nil {
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
	if err := gates.RegisterJobGate(r, gates.JobGateDeps{Jobs: reader, Applications: f.apps}); err != nil {
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
	f.apps.set(syncedApp, nil)
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
	f.apps.set(syncedApp, nil)
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
	f.apps.set(syncedApp, nil)
	reader := &fakeJobs{}
	reader.set(gates.Job{}, errors.New("kube api: 403 Forbidden"))
	h := newHarness(t, jobPlan(t, reader, f))

	out := mustEvaluate(t, h, deployment(core.KindDeploy), core.GatePhaseAfter)
	res := out.Results[1]
	if res.Status != core.GatePending || !strings.Contains(res.Message, "evaluation error: read Job web-prod/smoke-test: kube api: 403 Forbidden") {
		t.Errorf("job gate = %s %q, want pending with the evaluation error", res.Status, res.Message)
	}
}

func TestJobGateFreshness(t *testing.T) {
	t.Parallel()

	operation := func(phase string, startedAt time.Time) gates.Application {
		app := syncedApp
		app.OperationPhase, app.OperationStartedAt = phase, startedAt
		return app
	}
	afterJob := jobStarted.Add(time.Minute)
	succeeded := hookJob(func(j *gates.Job) { j.Complete, j.Succeeded = true, 1 })
	tests := []struct {
		name        string
		app         gates.Application
		appErr      error
		job         gates.Job
		jobErr      error
		wantStatus  core.GateStatus
		wantState   string
		wantMessage string
	}{
		{
			name:        "created in the second the sync started passes",
			app:         operation(gates.OperationSucceeded, succeeded.CreatedAt),
			job:         succeeded,
			wantStatus:  core.GatePassed,
			wantState:   "succeeded",
			wantMessage: "Job web-prod/smoke-test succeeded",
		},
		{
			name:        "a success older than a selective sync is pending",
			app:         operation(gates.OperationSucceeded, afterJob),
			job:         succeeded,
			wantStatus:  core.GatePending,
			wantState:   "stale",
			wantMessage: "Job web-prod/smoke-test was created at 2026-11-24T10:00:00Z, before the latest Argo CD sync started at 2026-11-24T10:01:02Z; waiting for Argo CD to rerun it",
		},
		{
			name:        "a success older than a running sync is pending",
			app:         operation(gates.OperationRunning, afterJob),
			job:         succeeded,
			wantStatus:  core.GatePending,
			wantState:   "stale",
			wantMessage: "before the latest Argo CD sync started",
		},
		{
			name:        "a success older than a failed sync fails",
			app:         operation(gates.OperationFailed, afterJob),
			job:         succeeded,
			wantStatus:  core.GateFailed,
			wantState:   "sync_failed",
			wantMessage: "the latest Argo CD sync of Application argocd/web-prod ended Failed before Job web-prod/smoke-test ran for it",
		},
		{
			name:        "a success older than an errored sync fails",
			app:         operation(gates.OperationError, afterJob),
			job:         succeeded,
			wantStatus:  core.GateFailed,
			wantState:   "sync_failed",
			wantMessage: "ended Error before Job web-prod/smoke-test ran",
		},
		{
			name:        "a running Job older than a failed sync fails",
			app:         operation(gates.OperationFailed, afterJob),
			job:         hookJob(func(j *gates.Job) { j.Active = 1 }),
			wantStatus:  core.GateFailed,
			wantState:   "sync_failed",
			wantMessage: "ended Failed",
		},
		{
			name:        "a Job without a creation time is stale",
			app:         syncedApp,
			job:         hookJob(func(j *gates.Job) { j.Complete, j.CreatedAt = true, time.Time{} }),
			wantStatus:  core.GatePending,
			wantState:   "stale",
			wantMessage: "before the latest Argo CD sync",
		},
		{
			name:        "a missing Job after a failed sync fails",
			app:         operation(gates.OperationFailed, syncStarted),
			jobErr:      gates.ErrJobNotFound,
			wantStatus:  core.GateFailed,
			wantState:   "sync_failed",
			wantMessage: "ended Failed before Job web-prod/smoke-test ran",
		},
		{
			name:        "a missing Job during a running sync is pending",
			app:         operation(gates.OperationRunning, syncStarted),
			jobErr:      gates.ErrJobNotFound,
			wantStatus:  core.GatePending,
			wantState:   "not_found",
			wantMessage: "Job web-prod/smoke-test not found; waiting for Argo CD to create it",
		},
		{
			name:        "no sync operation is pending",
			app:         operation("", time.Time{}),
			job:         succeeded,
			wantStatus:  core.GatePending,
			wantState:   "no_operation",
			wantMessage: "Argo CD reports no sync operation for Application argocd/web-prod, so Job web-prod/smoke-test cannot be tied to this deploy",
		},
		{
			name:        "the hook that failed the sync fails with its reason",
			app:         operation(gates.OperationFailed, syncStarted),
			job:         hookJob(func(j *gates.Job) { j.FailedCondition, j.Reason = true, "BackoffLimitExceeded" }),
			wantStatus:  core.GateFailed,
			wantState:   "failed",
			wantMessage: "Job web-prod/smoke-test failed: BackoffLimitExceeded",
		},
		{
			name:        "a fresh success passes when another hook failed the sync",
			app:         operation(gates.OperationFailed, syncStarted),
			job:         succeeded,
			wantStatus:  core.GatePassed,
			wantState:   "succeeded",
			wantMessage: "succeeded",
		},
		{
			name:        "a stale Job that is not a hook still fails as not a hook",
			app:         operation(gates.OperationSucceeded, afterJob),
			job:         hookJob(func(j *gates.Job) { j.Annotations, j.Complete = nil, true }),
			wantStatus:  core.GateFailed,
			wantState:   "not_a_hook",
			wantMessage: "is not an Argo CD PostSync hook",
		},
		{
			name:        "a stale Job being replaced is pending",
			app:         operation(gates.OperationRunning, afterJob),
			job:         hookJob(func(j *gates.Job) { j.Deleting, j.Complete = true, true }),
			wantStatus:  core.GatePending,
			wantState:   "terminating",
			wantMessage: "is being replaced",
		},
		{
			name:        "a missing Application is pending",
			appErr:      fmt.Errorf("get: %w", gates.ErrApplicationNotFound),
			job:         succeeded,
			wantStatus:  core.GatePending,
			wantState:   "no_application",
			wantMessage: "Argo CD Application argocd/web-prod not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeJobs{}
			reader.set(tt.job, tt.jobErr)
			apps := &fakeApplications{}
			apps.set(tt.app, tt.appErr)
			ev := evaluateGate(t, jobGateWith(t, reader, apps, jobConfig("")), deployment(core.KindDeploy), nil)
			if ev.Status != tt.wantStatus || !strings.Contains(ev.Message, tt.wantMessage) {
				t.Fatalf("Evaluate = %s %q, want %s %q", ev.Status, ev.Message, tt.wantStatus, tt.wantMessage)
			}
			evidence := evidenceMap(t, ev)
			if evidence["state"] != tt.wantState || evidence["application"] != "argocd/web-prod" {
				t.Errorf("evidence = %s, want state %s for argocd/web-prod", ev.Evidence, tt.wantState)
			}
			if apps.namespace != "argocd" || apps.name != "web-prod" {
				t.Errorf("read Application %s/%s, want argocd/web-prod", apps.namespace, apps.name)
			}
		})
	}
}

func TestJobGateApplicationReadError(t *testing.T) {
	t.Parallel()

	reader := &fakeJobs{}
	reader.set(hookJob(func(j *gates.Job) { j.Complete = true }), nil)
	apps := &fakeApplications{}
	forbidden := errors.New("kube api: 403 Forbidden")
	apps.set(gates.Application{}, forbidden)
	g := jobGateWith(t, reader, apps, jobConfig(""))
	_, err := g.Evaluate(t.Context(), gates.Request{Deployment: deployment(core.KindDeploy), Phase: core.GatePhaseAfter})
	if !errors.Is(err, forbidden) || !strings.Contains(err.Error(), "read Argo CD Application argocd/web-prod") {
		t.Errorf("Evaluate error = %v, want the Application read error", err)
	}
	if n := reader.callCount(); n != 0 {
		t.Errorf("read the Job %d times after the Application read failed", n)
	}
}
