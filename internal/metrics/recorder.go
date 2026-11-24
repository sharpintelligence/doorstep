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
	rollbacks          *prometheus.CounterVec
	gateDuration       *prometheus.HistogramVec
}

func newRecorder() *Recorder {
	return &Recorder{
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
	}
}

func (r *Recorder) collectors() []prometheus.Collector {
	return []prometheus.Collector{r.deploymentsStarted, r.deploymentDuration, r.rollbacks, r.gateDuration}
}

func (r *Recorder) DeploymentStarted(app string) {
	r.deploymentsStarted.WithLabelValues(app).Inc()
}

func (r *Recorder) DeploymentFinished(app string, outcome Outcome, duration time.Duration) {
	r.deploymentDuration.WithLabelValues(app, outcome.label()).Observe(duration.Seconds())
}

func (r *Recorder) Rollback(app string, reason RollbackReason) {
	r.rollbacks.WithLabelValues(app, reason.label()).Inc()
}

func (r *Recorder) GateEvaluated(app string, gate GateKind, phase Phase, result GateResult, duration time.Duration) {
	r.gateDuration.WithLabelValues(app, gate.label(), phase.label(), result.label()).Observe(duration.Seconds())
}

func (r *Recorder) PrimeApp(app string) {
	r.deploymentsStarted.WithLabelValues(app)
	for _, outcome := range allOutcomes {
		r.deploymentDuration.WithLabelValues(app, outcome.label())
	}
	for _, reason := range allReasons {
		r.rollbacks.WithLabelValues(app, reason.label())
	}
	for _, gate := range allGates {
		for _, phase := range allPhases {
			for _, result := range allResults {
				r.gateDuration.WithLabelValues(app, gate.label(), phase.label(), result.label())
			}
		}
	}
}
