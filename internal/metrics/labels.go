package metrics

const unknownLabel = "unknown"

type Outcome string

const (
	OutcomeSucceeded      Outcome = "succeeded"
	OutcomeFailed         Outcome = "failed"
	OutcomeRolledBack     Outcome = "rolled_back"
	OutcomeRollbackFailed Outcome = "rollback_failed"
	OutcomeCancelled      Outcome = "cancelled"
	OutcomeSuperseded     Outcome = "superseded"
)

func (o Outcome) label() string {
	switch o {
	case OutcomeSucceeded, OutcomeFailed, OutcomeRolledBack, OutcomeRollbackFailed, OutcomeCancelled, OutcomeSuperseded:
		return string(o)
	default:
		return unknownLabel
	}
}

type RollbackReason string

const (
	RollbackReasonGateFailed RollbackReason = "gate_failed"
	RollbackReasonSyncFailed RollbackReason = "sync_failed"
	RollbackReasonManual     RollbackReason = "manual"
)

func (r RollbackReason) label() string {
	switch r {
	case RollbackReasonGateFailed, RollbackReasonSyncFailed, RollbackReasonManual:
		return string(r)
	default:
		return unknownLabel
	}
}

type Phase string

const (
	PhaseDeploy   Phase = "deploy"
	PhaseRollback Phase = "rollback"
)

func (p Phase) label() string {
	switch p {
	case PhaseDeploy, PhaseRollback:
		return string(p)
	default:
		return unknownLabel
	}
}

type GateResult string

const (
	GateResultPassed GateResult = "passed"
	GateResultFailed GateResult = "failed"
	GateResultError  GateResult = "error"
)

func (r GateResult) label() string {
	switch r {
	case GateResultPassed, GateResultFailed, GateResultError:
		return string(r)
	default:
		return unknownLabel
	}
}

type GateKind string

const (
	GateKindCI       GateKind = "ci"
	GateKindLock     GateKind = "lock"
	GateKindArgoCD   GateKind = "argocd"
	GateKindSoak     GateKind = "soak"
	GateKindMetrics  GateKind = "metrics"
	GateKindManual   GateKind = "manual"
	GateKindSchedule GateKind = "schedule"
)

func (k GateKind) label() string {
	switch k {
	case GateKindCI, GateKindLock, GateKindArgoCD, GateKindSoak, GateKindMetrics, GateKindManual, GateKindSchedule:
		return string(k)
	default:
		return unknownLabel
	}
}

type NotificationType string

const (
	NotificationSlack   NotificationType = "slack"
	NotificationWebhook NotificationType = "webhook"
	NotificationGitHub  NotificationType = "github"
)

func (t NotificationType) label() string {
	switch t {
	case NotificationSlack, NotificationWebhook, NotificationGitHub:
		return string(t)
	default:
		return unknownLabel
	}
}

type NotificationResult string

const (
	NotificationSent    NotificationResult = "sent"
	NotificationFailed  NotificationResult = "failed"
	NotificationDropped NotificationResult = "dropped"
)

func (r NotificationResult) label() string {
	switch r {
	case NotificationSent, NotificationFailed, NotificationDropped:
		return string(r)
	default:
		return unknownLabel
	}
}

type WebhookEvent string

const (
	WebhookEventPush                     WebhookEvent = "push"
	WebhookEventCheckRun                 WebhookEvent = "check_run"
	WebhookEventCheckSuite               WebhookEvent = "check_suite"
	WebhookEventStatus                   WebhookEvent = "status"
	WebhookEventInstallation             WebhookEvent = "installation"
	WebhookEventInstallationRepositories WebhookEvent = "installation_repositories"
	WebhookEventPing                     WebhookEvent = "ping"
	WebhookEventOther                    WebhookEvent = "other"
)

func (e WebhookEvent) label() string {
	switch e {
	case WebhookEventPush, WebhookEventCheckRun, WebhookEventCheckSuite, WebhookEventStatus,
		WebhookEventInstallation, WebhookEventInstallationRepositories, WebhookEventPing, WebhookEventOther:
		return string(e)
	default:
		return string(WebhookEventOther)
	}
}

type WebhookResult string

const (
	WebhookProcessed    WebhookResult = "processed"
	WebhookIgnored      WebhookResult = "ignored"
	WebhookDuplicate    WebhookResult = "duplicate"
	WebhookInProgress   WebhookResult = "in_progress"
	WebhookRejected     WebhookResult = "rejected"
	WebhookUnauthorized WebhookResult = "unauthorized"
	WebhookFailed       WebhookResult = "failed"
)

func (r WebhookResult) label() string {
	switch r {
	case WebhookProcessed, WebhookIgnored, WebhookDuplicate, WebhookInProgress, WebhookRejected, WebhookUnauthorized, WebhookFailed:
		return string(r)
	default:
		return unknownLabel
	}
}

type PollResult string

const (
	PollSucceeded PollResult = "succeeded"
	PollFailed    PollResult = "failed"
)

func (r PollResult) label() string {
	switch r {
	case PollSucceeded, PollFailed:
		return string(r)
	default:
		return unknownLabel
	}
}

var (
	allOutcomes = []Outcome{OutcomeSucceeded, OutcomeFailed, OutcomeRolledBack, OutcomeRollbackFailed, OutcomeCancelled, OutcomeSuperseded}
	allReasons  = []RollbackReason{RollbackReasonGateFailed, RollbackReasonSyncFailed, RollbackReasonManual}
	allPhases   = []Phase{PhaseDeploy, PhaseRollback}
	allResults  = []GateResult{GateResultPassed, GateResultFailed, GateResultError}
	allGates    = []GateKind{GateKindCI, GateKindLock, GateKindArgoCD, GateKindSoak, GateKindMetrics, GateKindManual, GateKindSchedule}

	allNotificationTypes   = []NotificationType{NotificationSlack, NotificationWebhook, NotificationGitHub}
	allNotificationResults = []NotificationResult{NotificationSent, NotificationFailed, NotificationDropped}

	allWebhookEvents = []WebhookEvent{
		WebhookEventPush, WebhookEventCheckRun, WebhookEventCheckSuite, WebhookEventStatus,
		WebhookEventInstallation, WebhookEventInstallationRepositories, WebhookEventPing, WebhookEventOther,
	}
	allWebhookResults = []WebhookResult{WebhookProcessed, WebhookIgnored, WebhookDuplicate, WebhookInProgress, WebhookRejected, WebhookUnauthorized, WebhookFailed}
	allPollResults    = []PollResult{PollSucceeded, PollFailed}
)
