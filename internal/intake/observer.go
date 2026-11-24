package intake

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

type PollResult string

const (
	PollSucceeded PollResult = "succeeded"
	PollFailed    PollResult = "failed"
)

type Observer interface {
	WebhookDelivery(event string, result WebhookResult)
	PollCycle(result PollResult)
	MigrationDetectionFailed(app string)
}

type nopObserver struct{}

func (nopObserver) WebhookDelivery(string, WebhookResult) {}

func (nopObserver) PollCycle(PollResult) {}

func (nopObserver) MigrationDetectionFailed(string) {}
