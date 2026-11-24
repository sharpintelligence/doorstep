package gates

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

const (
	argoCDHookAnnotation = "argocd.argoproj.io/hook"
	helmHookAnnotation   = "helm.sh/hook"

	jobStateNotFound    = "not_found"
	jobStateTerminating = "terminating"
	jobStateNotAHook    = "not_a_hook"
	jobStateSucceeded   = "succeeded"
	jobStateFailed      = "failed"
	jobStateSuspended   = "suspended"
	jobStateActive      = "active"
)

type JobGateDeps struct {
	Jobs JobReader
}

func RegisterJobGate(r *Registry, deps JobGateDeps) error {
	switch {
	case r == nil:
		return errors.New("register job gate: no registry")
	case deps.Jobs == nil:
		return errors.New("register job gate: the job gate needs a job reader")
	}
	if err := r.Register(config.GateJob, Definition{Kind: metrics.GateKindJob, SkipOnRollback: true, New: deps.newJob}); err != nil {
		return fmt.Errorf("register job gate: %w", err)
	}
	return nil
}

type jobRef struct {
	namespace string
	name      string
}

func (j jobRef) String() string {
	return j.namespace + "/" + j.name
}

type jobGate struct {
	ref         jobRef
	application applicationRef
	logs        config.JobGate
	reader      JobReader
}

type jobEvidence struct {
	Job          string `json:"job"`
	Found        bool   `json:"found"`
	UID          string `json:"uid,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	Hook         bool   `json:"hook"`
	State        string `json:"state"`
	Active       int32  `json:"active"`
	Succeeded    int32  `json:"succeeded"`
	Failed       int32  `json:"failed"`
	BackoffLimit int32  `json:"backoff_limit"`
	StartedAt    string `json:"started_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	LogsURL      string `json:"logs_url,omitempty"`
}

func (d JobGateDeps) newJob(app config.App, gate config.Gate) (Gate, error) {
	if gate.Job == nil {
		return nil, errors.New("job gate has no job settings")
	}
	return jobGate{
		ref:         jobRef{namespace: gate.Job.Namespace, name: gate.Job.Name},
		application: applicationOf(app),
		logs:        *gate.Job,
		reader:      d.Jobs,
	}, nil
}

func (g jobGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	job, err := g.reader.Job(ctx, g.ref.namespace, g.ref.name)
	found := true
	switch {
	case errors.Is(err, ErrJobNotFound):
		found = false
	case err != nil:
		return Evaluation{}, fmt.Errorf("read Job %s: %w", g.ref, err)
	}
	ev := jobEvidence{Job: g.ref.String(), Found: found, State: jobStateNotFound}
	status := core.GatePending
	message := fmt.Sprintf("Job %s not found; waiting for Argo CD to create it", g.ref)
	if found {
		ev = g.evidence(job)
		status, ev.State, message = judgeJob(g.ref, job)
	}
	ev.LogsURL = g.logsURL(req.Deployment, job.UID)
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}

func judgeJob(ref jobRef, job Job) (core.GateStatus, string, string) {
	switch {
	case job.Deleting:
		return core.GatePending, jobStateTerminating, fmt.Sprintf("Job %s is being replaced; waiting for Argo CD to recreate it", ref)
	case !isPostSyncHook(job.Annotations):
		return core.GateFailed, jobStateNotAHook, fmt.Sprintf("Job %s is not an Argo CD PostSync hook, so it does not rerun on each deploy", ref)
	case job.Complete:
		message := fmt.Sprintf("Job %s succeeded", ref)
		if d, ok := jobRuntime(job); ok {
			message += " in " + d.String()
		}
		return core.GatePassed, jobStateSucceeded, message
	case job.FailedCondition:
		return core.GateFailed, jobStateFailed, withDetail(withDetail(fmt.Sprintf("Job %s failed", ref), job.Reason), job.Message)
	case job.Suspended:
		return core.GatePending, jobStateSuspended, fmt.Sprintf("Job %s is suspended", ref)
	default:
		return core.GatePending, jobStateActive, fmt.Sprintf("Job %s is running: %d active, %d failed of backoff limit %d", ref, job.Active, job.Failed, job.BackoffLimit)
	}
}

func jobRuntime(job Job) (time.Duration, bool) {
	if job.StartedAt.IsZero() || job.CompletedAt.IsZero() || job.CompletedAt.Before(job.StartedAt) {
		return 0, false
	}
	return job.CompletedAt.Sub(job.StartedAt).Round(time.Second), true
}

func isPostSyncHook(annotations map[string]string) bool {
	if hooks, ok := annotations[argoCDHookAnnotation]; ok && strings.TrimSpace(hooks) != "" {
		return hasToken(hooks, "PostSync")
	}
	return hasToken(annotations[helmHookAnnotation], "post-install", "post-upgrade")
}

func hasToken(list string, want ...string) bool {
	for token := range strings.SplitSeq(list, ",") {
		for _, w := range want {
			if strings.TrimSpace(token) == w {
				return true
			}
		}
	}
	return false
}

func (g jobGate) evidence(job Job) jobEvidence {
	return jobEvidence{
		Job:          g.ref.String(),
		Found:        true,
		UID:          clean(job.UID),
		CreatedAt:    optionalTimestamp(job.CreatedAt),
		Hook:         isPostSyncHook(job.Annotations),
		Active:       job.Active,
		Succeeded:    job.Succeeded,
		Failed:       job.Failed,
		BackoffLimit: job.BackoffLimit,
		StartedAt:    optionalTimestamp(job.StartedAt),
		CompletedAt:  optionalTimestamp(job.CompletedAt),
		Reason:       clean(job.Reason),
		Message:      clean(job.Message),
	}
}

func (g jobGate) logsURL(d core.Deployment, uid string) string {
	return g.logs.RenderLogsURL(map[string]string{
		"app":                d.Target.App,
		"namespace":          g.ref.namespace,
		"job":                g.ref.name,
		"uid":                uid,
		"deployment":         strconv.FormatInt(int64(d.ID), 10),
		"argocd_namespace":   g.application.namespace,
		"argocd_application": g.application.name,
	})
}

func optionalTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return timestamp(t)
}
