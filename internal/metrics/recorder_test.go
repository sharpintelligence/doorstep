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
		app, gate string
		phase     Phase
		result    GateResult
		duration  time.Duration
	}
	tests := []struct {
		name  string
		calls []call
		want  []histogramSample
	}{
		{
			name: "grouped by all labels",
			calls: []call{
				{"web", "health", PhaseDeploy, GateResultPassed, 200 * time.Millisecond},
				{"web", "health", PhaseDeploy, GateResultPassed, 300 * time.Millisecond},
				{"web", "soak", PhaseRollback, GateResultFailed, 10 * time.Minute},
				{"web", "metrics", PhaseDeploy, GateResultError, time.Second},
			},
			want: []histogramSample{
				{labels: map[string]string{"app": "web", "gate": "health", "phase": "deploy", "result": "passed"}, count: 2, sum: 0.5},
				{labels: map[string]string{"app": "web", "gate": "soak", "phase": "rollback", "result": "failed"}, count: 1, sum: 600},
				{labels: map[string]string{"app": "web", "gate": "metrics", "phase": "deploy", "result": "error"}, count: 1, sum: 1},
			},
		},
		{
			name:  "unknown phase and result are normalised",
			calls: []call{{"web", "health", Phase("x"), GateResult("y"), time.Second}},
			want: []histogramSample{
				{labels: map[string]string{"app": "web", "gate": "health", "phase": "unknown", "result": "unknown"}, count: 1, sum: 1},
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
