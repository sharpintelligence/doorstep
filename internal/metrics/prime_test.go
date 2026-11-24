package metrics

import "testing"

func TestRecorderPrimeApp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		apps       []string
		metric     string
		wantSeries int
	}{
		{name: "deployment durations per outcome", apps: []string{"web"}, metric: "doorstep_deployment_duration_seconds", wantSeries: 6},
		{name: "gate durations for every combination", apps: []string{"web"}, metric: "doorstep_gate_evaluation_duration_seconds", wantSeries: 36},
		{name: "several apps", apps: []string{"web", "api"}, metric: "doorstep_deployment_duration_seconds", wantSeries: 12},
		{name: "priming twice changes nothing", apps: []string{"web", "web"}, metric: "doorstep_deployment_duration_seconds", wantSeries: 6},
		{name: "nothing primed", metric: "doorstep_deployment_duration_seconds"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			for _, app := range tt.apps {
				m.Recorder().PrimeApp(app)
			}
			got := gatherHistograms(t, m, tt.metric)
			if len(got) != tt.wantSeries {
				t.Fatalf("got %d series, want %d", len(got), tt.wantSeries)
			}
			for _, sample := range got {
				if sample.count != 0 {
					t.Errorf("%v count = %d, want 0", sample.labels, sample.count)
				}
			}
		})
	}
}

func TestRecorderPrimeAppCounters(t *testing.T) {
	t.Parallel()

	m := New(Options{})
	m.Recorder().PrimeApp("web")
	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	counts := map[string]int{}
	for _, family := range families {
		counts[family.GetName()] = len(family.GetMetric())
	}
	if counts["doorstep_deployments_started_total"] != 1 {
		t.Errorf("deployments_started series = %d, want 1", counts["doorstep_deployments_started_total"])
	}
	if counts["doorstep_rollbacks_total"] != 3 {
		t.Errorf("rollbacks series = %d, want 3", counts["doorstep_rollbacks_total"])
	}
}
