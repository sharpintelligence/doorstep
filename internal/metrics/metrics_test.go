package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
)

func scrape(t *testing.T, m *Metrics) (int, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return rec.Code, string(body)
}

func TestHandler(t *testing.T) {
	t.Parallel()

	build := buildinfo.Info{Version: "v1.2.3", Commit: "abc123", GoVersion: "go1.99"}

	tests := []struct {
		name         string
		opts         Options
		setup        func(*Recorder)
		wantContains []string
		wantMissing  []string
	}{
		{
			name: "build info and go collector",
			opts: Options{Build: build},
			wantContains: []string{
				`doorstep_build_info{commit="abc123",go_version="go1.99",version="v1.2.3"} 1`,
				"go_goroutines",
			},
			wantMissing: []string{"doorstep_queue_depth"},
		},
		{
			name: "queue depth registered when provided",
			opts: Options{Build: build, QueueDepth: func(context.Context) (int, error) { return 3, nil }},
			wantContains: []string{
				"doorstep_queue_depth 3",
				"doorstep_queue_depth_errors_total 0",
			},
		},
		{
			name: "queue depth error still serves other metrics",
			opts: Options{Build: build, QueueDepth: func(context.Context) (int, error) { return 0, errors.New("db down") }},
			wantContains: []string{
				"doorstep_build_info{",
				"doorstep_queue_depth_errors_total 1",
			},
			wantMissing: []string{"doorstep_queue_depth "},
		},
		{
			name:  "recorded values are exposed",
			opts:  Options{Build: build},
			setup: func(r *Recorder) { r.DeploymentStarted("web") },
			wantContains: []string{
				`doorstep_deployments_started_total{app="web"} 1`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, _ := newBufferLogger()
			tt.opts.Logger = logger
			m := New(tt.opts)
			if tt.setup != nil {
				tt.setup(m.Recorder())
			}

			code, body := scrape(t, m)
			if code != http.StatusOK {
				t.Fatalf("status = %d, body %q", code, body)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
			for _, unwanted := range tt.wantMissing {
				if strings.Contains(body, unwanted) {
					t.Errorf("body unexpectedly contains %q", unwanted)
				}
			}
		})
	}
}

func TestQueueDepthErrorLogsWarning(t *testing.T) {
	t.Parallel()

	logger, logs := newBufferLogger()
	m := New(Options{
		Logger:     logger,
		QueueDepth: func(context.Context) (int, error) { return 0, errors.New("db down") },
	})
	if code, _ := scrape(t, m); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(logs.String(), "queue depth unavailable") {
		t.Errorf("logs %q missing warning", logs.String())
	}
}

func TestNewHasNoGlobalState(t *testing.T) {
	t.Parallel()

	first := New(Options{})
	second := New(Options{})
	first.Recorder().DeploymentStarted("web")

	if got := testutil.CollectAndCount(second.Recorder().deploymentsStarted); got != 0 {
		t.Errorf("second instance saw %d series from the first", got)
	}
}

func TestGatherAndLint(t *testing.T) {
	t.Parallel()

	m := New(Options{QueueDepth: func(context.Context) (int, error) { return 1, nil }})
	r := m.Recorder()
	r.DeploymentStarted("web")
	r.DeploymentFinished("web", OutcomeSucceeded, 0)
	r.Rollback("web", RollbackReasonManual)
	r.GateEvaluated("web", "health", PhaseDeploy, GateResultPassed, 0)

	problems, err := testutil.GatherAndLint(m.Gatherer())
	if err != nil {
		t.Fatalf("GatherAndLint: %v", err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "doorstep_") {
			t.Errorf("lint problem: %s: %s", p.Metric, p.Text)
		}
	}
}
