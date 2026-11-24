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

	jobStateNoApplication = "no_application"
	jobStateNotFound      = "not_found"
	jobStateTerminating   = "terminating"
	jobStateNotAHook      = "not_a_hook"
	jobStateNoOperation   = "no_operation"
	jobStateStale         = "stale"
	jobStateSyncFailed    = "sync_failed"
	jobStateSucceeded     = "succeeded"
	jobStateFailed        = "failed"
	jobStateSuspended     = "suspended"
	jobStateActive        = "active"
)

type JobGateDeps struct {
	Jobs         JobReader
	Applications ApplicationReader
}

func RegisterJobGate(r *Registry, deps JobGateDeps) error {
	switch {
	case r == nil:
		return errors.New("register job gate: no registry")
	case deps.Jobs == nil:
		return errors.New("register job gate: the job gate needs a job reader")
	case deps.Applications == nil:
		return errors.New("register job gate: the job gate needs an Argo CD application reader")
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
	ref          jobRef
	application  applicationRef
	logs         config.JobGate
	reader       JobReader
	applications ApplicationReader
}

type jobEvidence struct {
	Job                string `json:"job"`
	Found              bool   `json:"found"`
	UID                string `json:"uid,omitempty"`
	CreatedAt          string `json:"created_at,omitempty"`
	Hook               bool   `json:"hook"`
	State              string `json:"state"`
	Active             int32  `json:"active"`
	Succeeded          int32  `json:"succeeded"`
	Failed             int32  `json:"failed"`
	BackoffLimit       int32  `json:"backoff_limit"`
	StartedAt          string `json:"started_at,omitempty"`
	CompletedAt        string `json:"completed_at,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	Application        string `json:"application"`
	OperationPhase     string `json:"operation_phase,omitempty"`
	OperationStartedAt string `json:"operation_started_at,omitempty"`
	LogsURL            string `json:"logs_url,omitempty"`
}

type jobObservation struct {
	job   Job
	found bool
	app   Application
}

func (d JobGateDeps) newJob(app config.App, gate config.Gate) (Gate, error) {
	if gate.Job == nil {
		return nil, errors.New("job gate has no job settings")
	}
	return jobGate{
		ref:          jobRef{namespace: gate.Job.Namespace, name: gate.Job.Name},
		application:  applicationOf(app),
		logs:         *gate.Job,
		reader:       d.Jobs,
		applications: d.Applications,
	}, nil
}

func (g jobGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	app, err := g.applications.Application(ctx, g.application.namespace, g.application.name)
	if errors.Is(err, ErrApplicationNotFound) {
		ev := jobEvidence{Job: g.ref.String(), Application: g.application.String(), State: jobStateNoApplication}
		return g.evaluation(req, ev, core.GatePending, fmt.Sprintf("Argo CD Application %s not found", g.application))
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("read Argo CD Application %s: %w", g.application, err)
	}
	job, err := g.reader.Job(ctx, g.ref.namespace, g.ref.name)
	found := true
	switch {
	case errors.Is(err, ErrJobNotFound):
		found = false
	case err != nil:
		return Evaluation{}, fmt.Errorf("read Job %s: %w", g.ref, err)
	}
	o := jobObservation{job: job, found: found, app: app}
	ev := g.evidence(o)
	status, state, message := g.judge(o)
	ev.State = state
	return g.evaluation(req, ev, status, message)
}

func (g jobGate) evaluation(req Request, ev jobEvidence, status core.GateStatus, message string) (Evaluation, error) {
	ev.LogsURL = g.logsURL(req.Deployment, ev.UID)
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}

func (g jobGate) judge(o jobObservation) (core.GateStatus, string, string) {
	ref, job := g.ref, o.job
	switch {
	case o.found && job.Deleting:
		return core.GatePending, jobStateTerminating, fmt.Sprintf("Job %s is being replaced; waiting for Argo CD to recreate it", ref)
	case o.found && !isPostSyncHook(job.Annotations):
		return core.GateFailed, jobStateNotAHook, fmt.Sprintf("Job %s is not an Argo CD PostSync hook, so it does not rerun on each deploy", ref)
	case !o.found || o.app.OperationStartedAt.IsZero() || job.CreatedAt.Before(o.app.OperationStartedAt):
		return g.judgeMissingRun(o)
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

func (g jobGate) judgeMissingRun(o jobObservation) (core.GateStatus, string, string) {
	switch phase := o.app.OperationPhase; {
	case o.app.OperationStartedAt.IsZero():
		return core.GatePending, jobStateNoOperation, fmt.Sprintf("Argo CD reports no sync operation for Application %s, so Job %s cannot be tied to this deploy", g.application, g.ref)
	case phase == OperationFailed || phase == OperationError:
		return core.GateFailed, jobStateSyncFailed, fmt.Sprintf("the latest Argo CD sync of Application %s ended %s before Job %s ran for it", g.application, phase, g.ref)
	case !o.found:
		return core.GatePending, jobStateNotFound, fmt.Sprintf("Job %s not found; waiting for Argo CD to create it", g.ref)
	default:
		return core.GatePending, jobStateStale, fmt.Sprintf("Job %s was created at %s, before the latest Argo CD sync started at %s; waiting for Argo CD to rerun it",
			g.ref, timestamp(o.job.CreatedAt), timestamp(o.app.OperationStartedAt))
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

func (g jobGate) evidence(o jobObservation) jobEvidence {
	ev := jobEvidence{
		Job:                g.ref.String(),
		Found:              o.found,
		Application:        g.application.String(),
		OperationPhase:     clean(o.app.OperationPhase),
		OperationStartedAt: optionalTimestamp(o.app.OperationStartedAt),
	}
	if !o.found {
		return ev
	}
	job := o.job
	ev.UID = clean(job.UID)
	ev.CreatedAt = optionalTimestamp(job.CreatedAt)
	ev.Hook = isPostSyncHook(job.Annotations)
	ev.Active, ev.Succeeded, ev.Failed, ev.BackoffLimit = job.Active, job.Succeeded, job.Failed, job.BackoffLimit
	ev.StartedAt = optionalTimestamp(job.StartedAt)
	ev.CompletedAt = optionalTimestamp(job.CompletedAt)
	ev.Reason = clean(job.Reason)
	ev.Message = clean(job.Message)
	return ev
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
