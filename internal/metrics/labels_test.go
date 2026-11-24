package metrics

import "testing"

func TestLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"outcome succeeded", OutcomeSucceeded.label(), "succeeded"},
		{"outcome failed", OutcomeFailed.label(), "failed"},
		{"outcome rolled back", OutcomeRolledBack.label(), "rolled_back"},
		{"outcome rollback failed", OutcomeRollbackFailed.label(), "rollback_failed"},
		{"outcome cancelled", OutcomeCancelled.label(), "cancelled"},
		{"outcome superseded", OutcomeSuperseded.label(), "superseded"},
		{"outcome unknown", Outcome("exploded").label(), "unknown"},
		{"outcome empty", Outcome("").label(), "unknown"},
		{"reason gate failed", RollbackReasonGateFailed.label(), "gate_failed"},
		{"reason sync failed", RollbackReasonSyncFailed.label(), "sync_failed"},
		{"reason manual", RollbackReasonManual.label(), "manual"},
		{"reason unknown", RollbackReason("because").label(), "unknown"},
		{"phase deploy", PhaseDeploy.label(), "deploy"},
		{"phase rollback", PhaseRollback.label(), "rollback"},
		{"phase unknown", Phase("later").label(), "unknown"},
		{"result passed", GateResultPassed.label(), "passed"},
		{"result failed", GateResultFailed.label(), "failed"},
		{"result error", GateResultError.label(), "error"},
		{"gate ci", GateKindCI.label(), "ci"},
		{"gate lock", GateKindLock.label(), "lock"},
		{"gate argocd", GateKindArgoCD.label(), "argocd"},
		{"gate soak", GateKindSoak.label(), "soak"},
		{"gate metrics", GateKindMetrics.label(), "metrics"},
		{"gate manual", GateKindManual.label(), "manual"},
		{"gate schedule", GateKindSchedule.label(), "schedule"},
		{"gate unknown", GateKind("my-custom-gate").label(), "unknown"},
		{"gate empty", GateKind("").label(), "unknown"},
		{"result unknown", GateResult("maybe").label(), "unknown"},
		{"notification slack", NotificationSlack.label(), "slack"},
		{"notification webhook", NotificationWebhook.label(), "webhook"},
		{"notification github", NotificationGitHub.label(), "github"},
		{"notification type unknown", NotificationType("ntfy").label(), "unknown"},
		{"notification sent", NotificationSent.label(), "sent"},
		{"notification failed", NotificationFailed.label(), "failed"},
		{"notification dropped", NotificationDropped.label(), "dropped"},
		{"notification result unknown", NotificationResult("lost").label(), "unknown"},
		{"webhook push", WebhookEventPush.label(), "push"},
		{"webhook check run", WebhookEventCheckRun.label(), "check_run"},
		{"webhook check suite", WebhookEventCheckSuite.label(), "check_suite"},
		{"webhook status", WebhookEventStatus.label(), "status"},
		{"webhook installation", WebhookEventInstallation.label(), "installation"},
		{"webhook installation repositories", WebhookEventInstallationRepositories.label(), "installation_repositories"},
		{"webhook ping", WebhookEventPing.label(), "ping"},
		{"webhook other", WebhookEventOther.label(), "other"},
		{"webhook unhandled event", WebhookEvent("pull_request").label(), "other"},
		{"webhook empty event", WebhookEvent("").label(), "other"},
		{"webhook processed", WebhookProcessed.label(), "processed"},
		{"webhook ignored", WebhookIgnored.label(), "ignored"},
		{"webhook duplicate", WebhookDuplicate.label(), "duplicate"},
		{"webhook in progress", WebhookInProgress.label(), "in_progress"},
		{"webhook rejected", WebhookRejected.label(), "rejected"},
		{"webhook unauthorized", WebhookUnauthorized.label(), "unauthorized"},
		{"webhook failed", WebhookFailed.label(), "failed"},
		{"webhook result unknown", WebhookResult("lost").label(), "unknown"},
		{"poll succeeded", PollSucceeded.label(), "succeeded"},
		{"poll failed", PollFailed.label(), "failed"},
		{"poll result unknown", PollResult("skipped").label(), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("label() = %q, want %q", tt.got, tt.want)
			}
		})
	}
}
