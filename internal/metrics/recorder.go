package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "doorstep"

var (
	deploymentDurationBuckets = []float64{5, 10, 30, 60, 120, 300, 600, 900, 1800, 3600}
	gateDurationBuckets       = []float64{0.1, 0.5, 1, 5, 15, 60, 300, 900, 1800}
)

type Recorder struct {
	deploymentsStarted *prometheus.CounterVec
	deploymentDuration *prometheus.HistogramVec
	deploymentRetries  *prometheus.CounterVec
	rollbacks          *prometheus.CounterVec
	gateDuration       *prometheus.HistogramVec
	notifications      *prometheus.CounterVec
	webhookDeliveries  *prometheus.CounterVec
	pollCycles         *prometheus.CounterVec
	migrationFailures  *prometheus.CounterVec
}

func newRecorder() *Recorder {
	r := &Recorder{
		deploymentsStarted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "deployments_started_total",
			Help:      "Deployments that started, by application.",
		}, []string{"app"}),
		deploymentDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "deployment_duration_seconds",
			Help:      "Time from deployment start to a final outcome, by application and outcome.",
			Buckets:   deploymentDurationBuckets,
		}, []string{"app", "outcome"}),
		deploymentRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "deployment_retries_total",
			Help:      "Failed deployment attempts that doorstep retried on the same version, by application and failure class.",
		}, []string{"app", "class"}),
		rollbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rollbacks_total",
			Help:      "Rollbacks started, by application and reason.",
		}, []string{"app", "reason"}),
		gateDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "gate_evaluation_duration_seconds",
			Help:      "Time spent evaluating a gate, by application, gate kind, phase and result.",
			Buckets:   gateDurationBuckets,
		}, []string{"app", "gate", "phase", "result"}),
		notifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "notifications_total",
			Help:      "Notification deliveries, by target type and result.",
		}, []string{"type", "result"}),
		webhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "webhook_deliveries_total",
			Help:      "GitHub webhook deliveries received, by event and result.",
		}, []string{"event", "result"}),
		pollCycles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "intake_poll_cycles_total",
			Help:      "Intake poll cycles run by this replica, by result.",
		}, []string{"result"}),
		migrationFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "migration_detection_failures_total",
			Help:      "Green commits not enqueued because their migration flags could not be read, by application.",
		}, []string{"app"}),
	}
	for _, t := range allNotificationTypes {
		for _, result := range allNotificationResults {
			r.notifications.WithLabelValues(t.label(), result.label())
		}
	}
	for _, event := range allWebhookEvents {
		for _, result := range allWebhookResults {
			r.webhookDeliveries.WithLabelValues(event.label(), result.label())
		}
	}
	for _, result := range allPollResults {
		r.pollCycles.WithLabelValues(result.label())
	}
	return r
}

func (r *Recorder) collectors() []prometheus.Collector {
	return []prometheus.Collector{r.deploymentsStarted, r.deploymentDuration, r.deploymentRetries, r.rollbacks, r.gateDuration, r.notifications, r.webhookDeliveries, r.pollCycles, r.migrationFailures}
}

func (r *Recorder) DeploymentStarted(app string) {
	r.deploymentsStarted.WithLabelValues(app).Inc()
}

func (r *Recorder) DeploymentFinished(app string, outcome Outcome, duration time.Duration) {
	r.deploymentDuration.WithLabelValues(app, outcome.label()).Observe(duration.Seconds())
}

func (r *Recorder) DeploymentRetried(app string, class FailureClass) {
	r.deploymentRetries.WithLabelValues(app, class.label()).Inc()
}

func (r *Recorder) Rollback(app string, reason RollbackReason) {
	r.rollbacks.WithLabelValues(app, reason.label()).Inc()
}

func (r *Recorder) GateEvaluated(app string, gate GateKind, phase Phase, result GateResult, duration time.Duration) {
	r.gateDuration.WithLabelValues(app, gate.label(), phase.label(), result.label()).Observe(duration.Seconds())
}

func (r *Recorder) NotificationDelivered(t NotificationType, result NotificationResult) {
	r.notifications.WithLabelValues(t.label(), result.label()).Inc()
}

func (r *Recorder) WebhookDelivery(event WebhookEvent, result WebhookResult) {
	r.webhookDeliveries.WithLabelValues(event.label(), result.label()).Inc()
}

func (r *Recorder) PollCycle(result PollResult) {
	r.pollCycles.WithLabelValues(result.label()).Inc()
}

func (r *Recorder) MigrationDetectionFailed(app string) {
	r.migrationFailures.WithLabelValues(app).Inc()
}

func (r *Recorder) PrimeApp(app string) {
	r.deploymentsStarted.WithLabelValues(app)
	r.migrationFailures.WithLabelValues(app)
	for _, outcome := range allOutcomes {
		r.deploymentDuration.WithLabelValues(app, outcome.label())
	}
	for _, reason := range allReasons {
		r.rollbacks.WithLabelValues(app, reason.label())
	}
	for _, class := range primedRetryClasses {
		r.deploymentRetries.WithLabelValues(app, class.label())
	}
	for _, gate := range allGates {
		for _, phase := range allPhases {
			for _, result := range allResults {
				r.gateDuration.WithLabelValues(app, gate.label(), phase.label(), result.label())
			}
		}
	}
}
