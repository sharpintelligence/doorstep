package argocd

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	MaxResources    = 20
	MaxMessageBytes = 512

	argoSynced          = "Synced"
	argoOutOfSync       = "OutOfSync"
	argoSyncFailed      = "SyncFailed"
	argoHookFailed      = "Failed"
	argoHookError       = "Error"
	argoHealthy         = "Healthy"
	argoProgressing     = "Progressing"
	argoDegraded        = "Degraded"
	argoSuspended       = "Suspended"
	argoMissing         = "Missing"
	conditionComparison = "ComparisonError"
	conditionSync       = "SyncError"
	conditionInvalid    = "InvalidSpecError"
)

func assess(app application, repository, pin string, contains func(revision string) bool, now time.Time) core.SyncStatus {
	revisions := app.revisions()
	status := core.SyncStatus{
		State:     syncState(app.Status.Sync.Status),
		Revision:  strings.Join(revisions, ", "),
		Revisions: slices.Clone(revisions),
		Health:    appHealth(app, now),
	}
	if op := currentOperation(app); op != nil {
		status.OperationPhase = op.Phase
		status.OperationMessage = cut(op.Message)
	}
	if pin == "" {
		return status
	}
	status.Reached = slices.ContainsFunc(candidateRevisions(app, repository), func(revision string) bool {
		if strings.EqualFold(revision, pin) {
			return true
		}
		return contains != nil && core.ValidateCommitSHA(revision) == nil && contains(revision)
	})
	if !status.Ready() {
		status.Failure = failure(app, status, pin)
	}
	return status
}

func candidateRevisions(app application, repository string) []string {
	if !app.multiSource() {
		return app.revisions()
	}
	revisions := app.Status.Sync.Revisions
	want := strings.ToLower(repository)
	var matched []string
	for i, source := range app.Spec.Sources {
		if i < len(revisions) && normaliseRepo(source.RepoURL) == want {
			matched = append(matched, revisions[i])
		}
	}
	if len(matched) > 0 {
		return matched
	}
	for _, revision := range revisions {
		if core.ValidateCommitSHA(revision) == nil {
			matched = append(matched, revision)
		}
	}
	return matched
}

func currentOperation(app application) *operationState {
	op := app.Status.OperationState
	if op == nil {
		return nil
	}
	if op.Phase == core.OperationRunning || op.Phase == core.OperationTerminating {
		return op
	}
	if op.SyncResult == nil {
		return nil
	}
	if app.multiSource() {
		if len(op.SyncResult.Revisions) > 0 && slices.Equal(op.SyncResult.Revisions, app.Status.Sync.Revisions) {
			return op
		}
		return nil
	}
	if op.SyncResult.Revision != "" && op.SyncResult.Revision == app.Status.Sync.Revision {
		return op
	}
	return nil
}

func appHealth(app application, now time.Time) core.Health {
	observed := now
	if t, err := time.Parse(time.RFC3339, app.Status.ReconciledAt); err == nil {
		observed = t
	}
	return core.Health{
		Status:     healthStatus(app.Status.Health.Status),
		Message:    cut(app.Status.Health.Message),
		Resources:  resources(app),
		ObservedAt: observed,
	}
}

func resources(app application) []core.ResourceStatus {
	var out []core.ResourceStatus
	index := make(map[[4]string]int)
	add := func(r core.ResourceStatus) {
		key := [4]string{r.Group, r.Kind, r.Namespace, r.Name}
		if i, ok := index[key]; ok {
			if out[i].Message == "" {
				out[i].Message = r.Message
			}
			return
		}
		if len(out) == MaxResources {
			return
		}
		index[key] = len(out)
		out = append(out, r)
	}
	if op := currentOperation(app); op != nil && op.SyncResult != nil {
		for _, r := range op.SyncResult.Resources {
			if r.Status != argoSyncFailed && r.HookPhase != argoHookFailed && r.HookPhase != argoHookError {
				continue
			}
			add(core.ResourceStatus{
				Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name,
				Health: core.HealthUnknown, Sync: r.Status, Message: cut(r.Message),
			})
		}
	}
	for _, r := range app.Status.Resources {
		health := core.HealthUnknown
		var message string
		if r.Health != nil {
			health = healthStatus(r.Health.Status)
			message = r.Health.Message
		}
		if (r.Health == nil || health == core.HealthHealthy) && r.Status != argoOutOfSync {
			continue
		}
		add(core.ResourceStatus{
			Group: r.Group, Kind: r.Kind, Namespace: r.Namespace, Name: r.Name,
			Health: health, Sync: r.Status, Message: cut(message),
		})
	}
	return out
}

func failure(app application, status core.SyncStatus, pin string) *core.SyncFailure {
	automated := app.automatedSync()
	errorCondition := firstErrorCondition(app)
	var reason core.SyncFailureReason
	var message string
	switch {
	case status.Reached && status.OperationPhase == core.OperationFailed:
		reason, message = core.SyncReasonFailed, withDetail("sync operation failed", status.OperationMessage)
	case !status.Reached:
		reason, message = core.SyncReasonNotObserved, notObserved(status.Revision, pin)
		if errorCondition != nil {
			message += "; " + errorCondition.Type + ": " + errorCondition.Message
		}
	case status.OperationPhase == core.OperationError:
		reason, message = core.SyncReasonError, withDetail("sync operation errored", status.OperationMessage)
	case errorCondition != nil:
		reason, message = core.SyncReasonError, withDetail(errorCondition.Type, errorCondition.Message)
	case status.OperationInProgress():
		reason, message = core.SyncReasonRunning, withDetail("sync operation is still running", status.OperationMessage)
	case status.State != core.SyncSynced:
		reason, message = core.SyncReasonOutOfSync, "application is "+orUnknown(app.Status.Sync.Status)
		if !automated {
			message += "; automated sync is disabled, so the application must be synced by hand"
		}
	case status.Health.Status == core.HealthDegraded:
		reason, message = core.SyncReasonDegraded, healthMessage(status.Health)
	case status.Health.Status == core.HealthMissing:
		reason, message = core.SyncReasonMissing, healthMessage(status.Health)
	case status.Health.Status == core.HealthSuspended:
		reason, message = core.SyncReasonSuspended, healthMessage(status.Health)
	default:
		reason, message = core.SyncReasonProgressing, healthMessage(status.Health)
	}
	return &core.SyncFailure{
		Reason:    reason,
		Class:     reason.Class(automated),
		Message:   cut(message),
		Resources: slices.Clone(status.Health.Resources),
	}
}

func notObserved(revision, pin string) string {
	if revision == "" {
		return "Argo CD has not reported a synced revision yet; waiting for " + pin
	}
	return "synced revision " + revision + " does not contain " + pin + " yet"
}

func healthMessage(h core.Health) string {
	message := "application health is " + string(h.Status)
	if h.Message != "" {
		message += ": " + h.Message
	}
	for _, r := range h.Resources {
		if r.Health != core.HealthHealthy && r.Message != "" {
			return message + "; " + r.Kind + " " + r.Name + ": " + r.Message
		}
	}
	return message
}

func firstErrorCondition(app application) *condition {
	for i, c := range app.Status.Conditions {
		switch c.Type {
		case conditionComparison, conditionSync, conditionInvalid:
			return &app.Status.Conditions[i]
		}
	}
	return nil
}

func syncState(status string) core.SyncState {
	switch status {
	case argoSynced:
		return core.SyncSynced
	case argoOutOfSync:
		return core.SyncOutOfSync
	default:
		return core.SyncUnknown
	}
}

func healthStatus(status string) core.HealthStatus {
	switch status {
	case argoHealthy:
		return core.HealthHealthy
	case argoProgressing:
		return core.HealthProgressing
	case argoDegraded:
		return core.HealthDegraded
	case argoSuspended:
		return core.HealthSuspended
	case argoMissing:
		return core.HealthMissing
	default:
		return core.HealthUnknown
	}
}

func withDetail(summary, detail string) string {
	if detail == "" {
		return summary
	}
	return summary + ": " + detail
}

func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

func cut(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= MaxMessageBytes {
		return s
	}
	end := MaxMessageBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}
