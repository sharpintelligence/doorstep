package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestRecorderCounters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		calls func(*Recorder)
		want  string
	}{
		{
			name: "deployments started per app",
			calls: func(r *Recorder) {
				r.DeploymentStarted("web")
				r.DeploymentStarted("web")
				r.DeploymentStarted("api")
			},
			want: `
# HELP doorstep_deployments_started_total Deployments that started, by application.
# TYPE doorstep_deployments_started_total counter
doorstep_deployments_started_total{app="api"} 1
doorstep_deployments_started_total{app="web"} 2
`,
		},
		{
			name: "notifications by type and result, primed at zero",
			calls: func(r *Recorder) {
				r.NotificationDelivered(NotificationSlack, NotificationSent)
				r.NotificationDelivered(NotificationSlack, NotificationSent)
				r.NotificationDelivered(NotificationWebhook, NotificationFailed)
				r.NotificationDelivered(NotificationGitHub, NotificationDropped)
				r.NotificationDelivered(NotificationType("email"), NotificationResult("lost"))
			},
			want: `
# HELP doorstep_notifications_total Notification deliveries, by target type and result.
# TYPE doorstep_notifications_total counter
doorstep_notifications_total{result="dropped",type="github"} 1
doorstep_notifications_total{result="dropped",type="slack"} 0
doorstep_notifications_total{result="dropped",type="webhook"} 0
doorstep_notifications_total{result="failed",type="github"} 0
doorstep_notifications_total{result="failed",type="slack"} 0
doorstep_notifications_total{result="failed",type="webhook"} 1
doorstep_notifications_total{result="sent",type="github"} 0
doorstep_notifications_total{result="sent",type="slack"} 2
doorstep_notifications_total{result="sent",type="webhook"} 0
doorstep_notifications_total{result="unknown",type="unknown"} 1
`,
		},
		{
			name: "intake poll cycles by result, primed at zero",
			calls: func(r *Recorder) {
				r.PollCycle(PollSucceeded)
				r.PollCycle(PollSucceeded)
				r.PollCycle(PollResult("odd"))
			},
			want: `
# HELP doorstep_intake_poll_cycles_total Intake poll cycles run by this replica, by result.
# TYPE doorstep_intake_poll_cycles_total counter
doorstep_intake_poll_cycles_total{result="failed"} 0
doorstep_intake_poll_cycles_total{result="succeeded"} 2
doorstep_intake_poll_cycles_total{result="unknown"} 1
`,
		},
		{
			name: "rollbacks per app and reason",
			calls: func(r *Recorder) {
				r.Rollback("web", RollbackReasonGateFailed)
				r.Rollback("web", RollbackReasonGateFailed)
				r.Rollback("web", RollbackReasonManual)
				r.Rollback("web", RollbackReason("weird"))
			},
			want: `
# HELP doorstep_rollbacks_total Rollbacks started, by application and reason.
# TYPE doorstep_rollbacks_total counter
doorstep_rollbacks_total{app="web",reason="gate_failed"} 2
doorstep_rollbacks_total{app="web",reason="manual"} 1
doorstep_rollbacks_total{app="web",reason="unknown"} 1
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			tt.calls(m.Recorder())
			name := firstMetricName(tt.want)
			if err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(tt.want), name); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestRecorderWebhookDeliveries(t *testing.T) {
	t.Parallel()

	m := New(Options{})
	r := m.Recorder()
	r.WebhookDelivery(WebhookEventPush, WebhookProcessed)
	r.WebhookDelivery(WebhookEventPush, WebhookProcessed)
	r.WebhookDelivery(WebhookEventStatus, WebhookDuplicate)
	r.WebhookDelivery(WebhookEvent("pull_request"), WebhookIgnored)
	r.WebhookDelivery(WebhookEvent("deployment"), WebhookIgnored)
	r.WebhookDelivery(WebhookEventPing, WebhookResult("weird"))

	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "doorstep_webhook_deliveries_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := labelMap(metric.GetLabel())
			got[labels["event"]+"/"+labels["result"]] = metric.GetCounter().GetValue()
		}
	}
	if want := len(allWebhookEvents)*len(allWebhookResults) + 1; len(got) != want {
		t.Errorf("series = %d, want %d primed plus one unknown result", len(got), want)
	}
	want := map[string]float64{
		"push/processed":   2,
		"status/duplicate": 1,
		"other/ignored":    2,
		"ping/unknown":     1,
		"push/failed":      0,
		"push/in_progress": 0,
		"other/rejected":   0,
	}
	for key, value := range want {
		if v, ok := got[key]; !ok || v != value {
			t.Errorf("%s = %v (present %t), want %v", key, v, ok, value)
		}
	}
}

func firstMetricName(exposition string) string {
	for line := range strings.SplitSeq(exposition, "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, _, _ := strings.Cut(rest, " ")
			return name
		}
	}
	return ""
}

type histogramSample struct {
	labels map[string]string
	count  uint64
	sum    float64
}

func gatherHistograms(t *testing.T, m *Metrics, name string) []histogramSample {
	t.Helper()
	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var samples []histogramSample
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			samples = append(samples, histogramSample{
				labels: labelMap(metric.GetLabel()),
				count:  metric.GetHistogram().GetSampleCount(),
				sum:    metric.GetHistogram().GetSampleSum(),
			})
		}
	}
	return samples
}

func labelMap(pairs []*dto.LabelPair) map[string]string {
	labels := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		labels[pair.GetName()] = pair.GetValue()
	}
	return labels
}

func findSample(samples []histogramSample, labels map[string]string) (histogramSample, bool) {
	for _, s := range samples {
		if len(s.labels) != len(labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if s.labels[k] != v {
				match = false
			}
		}
		if match {
			return s, true
		}
	}
	return histogramSample{}, false
}

func TestRecorderDeploymentFinished(t *testing.T) {
	t.Parallel()

	type call struct {
		app      string
		outcome  Outcome
		duration time.Duration
	}
	tests := []struct {
		name  string
		calls []call
		want  []histogramSample
	}{
		{
			name: "grouped by app and outcome",
			calls: []call{
				{"web", OutcomeSucceeded, 30 * time.Second},
				{"web", OutcomeSucceeded, 90 * time.Second},
				{"web", OutcomeRolledBack, 2 * time.Minute},
				{"api", OutcomeFailed, 1500 * time.Millisecond},
			},
			want: []histogramSample{
				{labels: map[string]string{"app": "web", "outcome": "succeeded"}, count: 2, sum: 120},
				{labels: map[string]string{"app": "web", "outcome": "rolled_back"}, count: 1, sum: 120},
				{labels: map[string]string{"app": "api", "outcome": "failed"}, count: 1, sum: 1.5},
			},
		},
		{
			name:  "unknown outcome is normalised",
			calls: []call{{"web", Outcome("kaboom"), time.Second}, {"web", Outcome("other"), time.Second}},
			want: []histogramSample{
				{labels: map[string]string{"app": "web", "outcome": "unknown"}, count: 2, sum: 2},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			for _, c := range tt.calls {
				m.Recorder().DeploymentFinished(c.app, c.outcome, c.duration)
			}
			got := gatherHistograms(t, m, "doorstep_deployment_duration_seconds")
			if len(got) != len(tt.want) {
				t.Fatalf("got %d series, want %d: %+v", len(got), len(tt.want), got)
			}
			for _, want := range tt.want {
				sample, ok := findSample(got, want.labels)
				if !ok {
					t.Fatalf("series %v missing in %+v", want.labels, got)
				}
				if sample.count != want.count || sample.sum != want.sum {
					t.Errorf("%v = count %d sum %v, want count %d sum %v", want.labels, sample.count, sample.sum, want.count, want.sum)
				}
			}
		})
	}
}

func TestRecorderGateEvaluated(t *testing.T) {
	t.Parallel()

	type call struct {
		app      string
		gate     GateKind
		phase    Phase
		result   GateResult
		duration time.Duration
	}
	tests := []struct {
		name  string
		calls []call
		want  []histogramSample
	}{
		{
			name: "grouped by all labels",
			calls: []call{
				{"web", GateKindArgoCD, PhaseDeploy, GateResultPassed, 200 * time.Millisecond},
				{"web", GateKindArgoCD, PhaseDeploy, GateResultPassed, 300 * time.Millisecond},
				{"web", GateKindSoak, PhaseRollback, GateResultFailed, 10 * time.Minute},
				{"web", GateKindMetrics, PhaseDeploy, GateResultError, time.Second},
			},
			want: []histogramSample{
				{labels: map[string]string{"app": "web", "gate": "argocd", "phase": "deploy", "result": "passed"}, count: 2, sum: 0.5},
				{labels: map[string]string{"app": "web", "gate": "soak", "phase": "rollback", "result": "failed"}, count: 1, sum: 600},
				{labels: map[string]string{"app": "web", "gate": "metrics", "phase": "deploy", "result": "error"}, count: 1, sum: 1},
			},
		},
		{
			name:  "unknown phase and result are normalised",
			calls: []call{{"web", GateKindArgoCD, Phase("x"), GateResult("y"), time.Second}},
			want: []histogramSample{
				{labels: map[string]string{"app": "web", "gate": "argocd", "phase": "unknown", "result": "unknown"}, count: 1, sum: 1},
			},
		},
		{
			name:  "unknown gate kind is normalised",
			calls: []call{{"web", GateKind("user-defined-name"), PhaseDeploy, GateResultPassed, time.Second}},
			want: []histogramSample{
				{labels: map[string]string{"app": "web", "gate": "unknown", "phase": "deploy", "result": "passed"}, count: 1, sum: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			for _, c := range tt.calls {
				m.Recorder().GateEvaluated(c.app, c.gate, c.phase, c.result, c.duration)
			}
			got := gatherHistograms(t, m, "doorstep_gate_evaluation_duration_seconds")
			if len(got) != len(tt.want) {
				t.Fatalf("got %d series, want %d: %+v", len(got), len(tt.want), got)
			}
			for _, want := range tt.want {
				sample, ok := findSample(got, want.labels)
				if !ok {
					t.Fatalf("series %v missing in %+v", want.labels, got)
				}
				if sample.count != want.count || sample.sum != want.sum {
					t.Errorf("%v = count %d sum %v, want count %d sum %v", want.labels, sample.count, sample.sum, want.count, want.sum)
				}
			}
		})
	}
}
