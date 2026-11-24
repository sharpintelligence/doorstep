package gates_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/promquery"
)

type promReply struct {
	status int
	body   string
}

func vector(values ...string) promReply {
	series := make([]string, 0, len(values))
	for i, v := range values {
		series = append(series, fmt.Sprintf(`{"metric":{"i":"%d"},"value":[1764000000,%q]}`, i, v))
	}
	return promReply{status: http.StatusOK, body: `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}}`}
}

func scalar(v string) promReply {
	return promReply{status: http.StatusOK, body: fmt.Sprintf(`{"status":"success","data":{"resultType":"scalar","result":[1764000000,%q]}}`, v)}
}

func matrix(series ...[]string) promReply {
	out := make([]string, 0, len(series))
	for i, values := range series {
		points := make([]string, 0, len(values))
		for j, v := range values {
			points = append(points, fmt.Sprintf(`[%d,%q]`, 1764000000+60*j, v))
		}
		out = append(out, fmt.Sprintf(`{"metric":{"i":"%d"},"values":[%s]}`, i, strings.Join(points, ",")))
	}
	return promReply{status: http.StatusOK, body: `{"status":"success","data":{"resultType":"matrix","result":[` + strings.Join(out, ",") + `]}}`}
}

func apiError(status int, typ, msg string) promReply {
	return promReply{status: status, body: fmt.Sprintf(`{"status":"error","errorType":%q,"error":%q}`, typ, msg)}
}

type fakeProm struct {
	mu      sync.Mutex
	instant []promReply
	ranges  []promReply
	queries []url.Values
	rangeQs []url.Values
	block   chan struct{}
}

func newFakeProm(t *testing.T) (*fakeProm, *promquery.Client) {
	t.Helper()
	f := &fakeProm{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client, err := promquery.New(promquery.Options{
		BaseURL:   srv.URL + "/prom",
		Authorize: promquery.BearerAuth(func() (string, error) { return "prom-secret-token", nil }),
	})
	if err != nil {
		t.Fatalf("promquery.New: %v", err)
	}
	return f, client
}

func (f *fakeProm) serve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	block := f.block
	var reply promReply
	switch r.URL.Path {
	case "/prom/api/v1/query":
		f.queries = append(f.queries, r.PostForm)
		reply, f.instant = next(f.instant)
	case "/prom/api/v1/query_range":
		f.rangeQs = append(f.rangeQs, r.PostForm)
		reply, f.ranges = next(f.ranges)
	default:
		reply = promReply{status: http.StatusNotFound, body: "not found"}
	}
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	w.WriteHeader(reply.status)
	_, _ = io.WriteString(w, reply.body)
}

func next(queue []promReply) (promReply, []promReply) {
	if len(queue) == 0 {
		return promReply{status: http.StatusInternalServerError, body: "no scripted reply"}, queue
	}
	return queue[0], queue[1:]
}

func (f *fakeProm) script(instant, ranges []promReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instant = append(f.instant, instant...)
	f.ranges = append(f.ranges, ranges...)
}

func (f *fakeProm) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries), len(f.rangeQs)
}

func (f *fakeProm) lastRange(t *testing.T) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rangeQs) == 0 {
		t.Fatal("no range query was sent")
	}
	return f.rangeQs[len(f.rangeQs)-1]
}

func (f *fakeProm) lastQuery(t *testing.T) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatal("no instant query was sent")
	}
	return f.queries[len(f.queries)-1]
}

func promGate(mutate ...func(*config.Gate)) config.Gate {
	g := config.Gate{Type: config.GatePrometheus, Name: "error-rate", Server: "main", Query: "sum(rate(errors[5m]))", Condition: "< 0.05"}
	for _, m := range mutate {
		m(&g)
	}
	return g
}

func buildPrometheusGate(t *testing.T, q gates.MetricsQuerier, clock *fakeClock, g config.Gate) gates.Gate {
	t.Helper()
	r := gates.NewRegistry()
	if err := gates.RegisterPrometheus(r, gates.Prometheus{Servers: map[string]gates.MetricsQuerier{"main": q}, Now: clock.Now}); err != nil {
		t.Fatalf("RegisterPrometheus: %v", err)
	}
	plan, err := r.Plan(config.App{Name: "web", Gates: config.Gates{After: []config.Gate{g}}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan.After[0].Gate
}

func promDeployment(clock *fakeClock) core.Deployment {
	d := deployment(core.KindDeploy)
	d.CreatedAt = clock.Now().Add(-10 * time.Minute)
	started := clock.Now().Add(-5 * time.Minute)
	d.StartedAt = &started
	return d
}

type promStep struct {
	advance     time.Duration
	instant     []promReply
	ranges      []promReply
	wantStatus  core.GateStatus
	wantMessage string
	wantQueries int
	wantRanges  int
	wantFields  map[string]any
	wantSame    bool
}

func runPromSteps(t *testing.T, g config.Gate, d func(*fakeClock) core.Deployment, steps []promStep) (*fakeProm, gates.Evaluation) {
	t.Helper()
	clock := newClock()
	fake, client := newFakeProm(t)
	gate := buildPrometheusGate(t, client, clock, g)
	dep := d(clock)
	var previous *core.GateResult
	var ev gates.Evaluation
	for i, step := range steps {
		clock.Advance(step.advance)
		fake.script(step.instant, step.ranges)
		ev = evaluateGate(t, gate, dep, previous)
		if ev.Status != step.wantStatus {
			t.Fatalf("step %d: status = %s (%s), want %s", i, ev.Status, ev.Message, step.wantStatus)
		}
		if !strings.Contains(ev.Message, step.wantMessage) {
			t.Errorf("step %d: message = %q, want it to contain %q", i, ev.Message, step.wantMessage)
		}
		queries, ranges := fake.counts()
		if queries != step.wantQueries || ranges != step.wantRanges {
			t.Errorf("step %d: requests = %d instant, %d range; want %d, %d", i, queries, ranges, step.wantQueries, step.wantRanges)
		}
		if step.wantSame && (previous == nil || string(previous.Evidence) != string(ev.Evidence) || previous.Message != ev.Message) {
			t.Errorf("step %d: throttled call changed the result: %s %s", i, ev.Message, ev.Evidence)
		}
		fields := evidenceMap(t, ev)
		for k, want := range step.wantFields {
			if got := fields[k]; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("step %d: evidence %s = %v, want %v (evidence %s)", i, k, got, want, ev.Evidence)
			}
		}
		if strings.Contains(string(ev.Evidence), "prom-secret-token") || strings.Contains(ev.Message, "prom-secret-token") || strings.Contains(string(ev.Evidence), "127.0.0.1") {
			t.Errorf("step %d: result leaks the token or URL: %s %s", i, ev.Message, ev.Evidence)
		}
		previous = resultOf(ev)
	}
	return fake, ev
}

func TestPrometheusGateSampling(t *testing.T) {
	t.Parallel()

	ok, bad := vector("0.01"), vector("0.1")
	tests := []struct {
		name  string
		gate  config.Gate
		steps []promStep
	}{
		{
			name: "passes after the success threshold",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "1/5 samples passed; latest 0.01 (want < 0.05)", wantQueries: 1, wantFields: map[string]any{
					"server": "main", "query": "sum(rate(errors[5m]))", "condition": "< 0.05", "interval": "1m0s",
					"success_threshold": 5, "failure_threshold": 3, "started_at": "2026-11-23T12:00:00Z",
					"last_attempt_at": "2026-11-23T12:00:00Z", "consecutive_successes": 1, "consecutive_failures": 0,
					"samples": "[map[at:2026-11-23T12:00:00Z outcome:success value:0.01]]",
				}},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "2/5 samples passed", wantQueries: 2},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "3/5 samples passed", wantQueries: 3},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "4/5 samples passed", wantQueries: 4},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePassed, wantMessage: "5 consecutive samples passed; latest 0.01", wantQueries: 5, wantFields: map[string]any{
					"consecutive_successes": 5, "started_at": "2026-11-23T12:00:00Z", "last_attempt_at": "2026-11-23T12:04:00Z",
				}},
			},
		},
		{
			name: "fails after the failure threshold",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{bad}, wantStatus: core.GatePending, wantMessage: "1/3 samples failed; latest 0.1 (want < 0.05)", wantQueries: 1},
				{advance: time.Minute, instant: []promReply{bad}, wantStatus: core.GatePending, wantMessage: "2/3 samples failed", wantQueries: 2},
				{advance: time.Minute, instant: []promReply{bad}, wantStatus: core.GateFailed, wantMessage: "3 consecutive samples failed; latest 0.1 (want < 0.05)", wantQueries: 3, wantFields: map[string]any{"consecutive_failures": 3}},
			},
		},
		{
			name: "a success resets the failure streak and vice versa",
			gate: promGate(func(g *config.Gate) { g.SuccessThreshold = new(2); g.FailureThreshold = new(2) }),
			steps: []promStep{
				{instant: []promReply{bad}, wantStatus: core.GatePending, wantMessage: "1/2 samples failed", wantQueries: 1},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "1/2 samples passed", wantQueries: 2, wantFields: map[string]any{"consecutive_failures": 0, "consecutive_successes": 1}},
				{advance: time.Minute, instant: []promReply{bad}, wantStatus: core.GatePending, wantMessage: "1/2 samples failed", wantQueries: 3, wantFields: map[string]any{"consecutive_failures": 1, "consecutive_successes": 0}},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePending, wantQueries: 4},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePassed, wantMessage: "2 consecutive samples passed", wantQueries: 5},
			},
		},
		{
			name: "one sample within the interval",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "1/5", wantQueries: 1},
				{advance: 30 * time.Second, wantStatus: core.GatePending, wantMessage: "1/5", wantQueries: 1, wantSame: true},
				{advance: 29 * time.Second, wantStatus: core.GatePending, wantMessage: "1/5", wantQueries: 1, wantSame: true},
				{advance: time.Second, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "2/5", wantQueries: 2},
			},
		},
		{
			name: "initial delay",
			gate: promGate(func(g *config.Gate) { g.InitialDelay = config.Duration(2 * time.Minute) }),
			steps: []promStep{
				{wantStatus: core.GatePending, wantMessage: "first sample at 2026-11-23T12:02:00Z", wantFields: map[string]any{"started_at": "2026-11-23T12:00:00Z", "last_attempt_at": nil}},
				{advance: 119 * time.Second, wantStatus: core.GatePending, wantMessage: "first sample at 2026-11-23T12:02:00Z"},
				{advance: time.Second, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "1/5 samples passed", wantQueries: 1},
			},
		},
		{
			name: "no data is pending by default and keeps the streaks",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "1/5", wantQueries: 1},
				{advance: time.Minute, instant: []promReply{vector()}, wantStatus: core.GatePending, wantMessage: "no data for 1 sample; waiting for data", wantQueries: 2, wantFields: map[string]any{"consecutive_successes": 1, "consecutive_no_data": 1}},
				{advance: time.Minute, instant: []promReply{vector("NaN")}, wantStatus: core.GatePending, wantMessage: "no data for 2 consecutive samples; waiting for data", wantQueries: 3},
				{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "2/5 samples passed", wantQueries: 4, wantFields: map[string]any{"consecutive_no_data": 0}},
			},
		},
		{
			name: "no data fails when configured",
			gate: promGate(func(g *config.Gate) { g.NoData = config.NoDataFail }),
			steps: []promStep{
				{instant: []promReply{vector()}, wantStatus: core.GatePending, wantMessage: "1/3 samples failed; latest had no data", wantQueries: 1},
				{advance: time.Minute, instant: []promReply{vector("NaN")}, wantStatus: core.GatePending, wantMessage: "2/3 samples failed", wantQueries: 2},
				{advance: time.Minute, instant: []promReply{vector()}, wantStatus: core.GateFailed, wantMessage: "no data for 3 consecutive samples", wantQueries: 3, wantFields: map[string]any{
					"samples": "[map[at:2026-11-23T12:00:00Z outcome:no_data] map[at:2026-11-23T12:01:00Z outcome:no_data] map[at:2026-11-23T12:02:00Z outcome:no_data]]",
				}},
			},
		},
		{
			name: "mixed failures and no data fail on the value message",
			gate: promGate(func(g *config.Gate) { g.NoData = config.NoDataFail; g.FailureThreshold = new(2) }),
			steps: []promStep{
				{instant: []promReply{bad}, wantStatus: core.GatePending, wantQueries: 1},
				{advance: time.Minute, instant: []promReply{vector()}, wantStatus: core.GateFailed, wantMessage: "2 consecutive samples failed; latest had no data (want < 0.05)", wantQueries: 2},
			},
		},
		{
			name: "scalar results",
			gate: promGate(func(g *config.Gate) { g.SuccessThreshold = new(1) }),
			steps: []promStep{
				{instant: []promReply{scalar("0")}, wantStatus: core.GatePassed, wantMessage: "1 sample passed; latest 0", wantQueries: 1},
			},
		},
		{
			name: "infinite values are compared and recorded",
			gate: promGate(func(g *config.Gate) { g.FailureThreshold = new(1) }),
			steps: []promStep{
				{instant: []promReply{vector("+Inf")}, wantStatus: core.GateFailed, wantMessage: "1 sample failed; latest +Inf", wantQueries: 1, wantFields: map[string]any{
					"samples": "[map[at:2026-11-23T12:00:00Z outcome:failure value:+Inf]]",
				}},
			},
		},
		{
			name: "several series are a query problem",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{vector("1", "2")}, wantStatus: core.GatePending, wantMessage: "query failed: query returned 2 series; aggregate it to a single value", wantQueries: 1, wantFields: map[string]any{
					"last_error": "query failed: query returned 2 series; aggregate it to a single value", "last_attempt_at": "2026-11-23T12:00:00Z", "samples": "[]",
				}},
				{advance: 30 * time.Second, wantStatus: core.GatePending, wantMessage: "query returned 2 series", wantQueries: 1, wantSame: true},
			},
		},
		{
			name: "a range vector is a query problem",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{matrix([]string{"1"})}, wantStatus: core.GatePending, wantMessage: "query failed: query returned a matrix result; want a scalar or an instant vector", wantQueries: 1},
			},
		},
		{
			name: "server errors stay pending, are throttled and keep the streaks",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "1/5", wantQueries: 1},
				{advance: time.Minute, instant: []promReply{apiError(http.StatusServiceUnavailable, "unavailable", "overloaded")}, wantStatus: core.GatePending, wantMessage: "query failed: prometheus responded 503 Service Unavailable: unavailable: overloaded", wantQueries: 2, wantFields: map[string]any{"consecutive_successes": 1}},
				{advance: 30 * time.Second, wantStatus: core.GatePending, wantMessage: "query failed", wantQueries: 2, wantSame: true},
				{advance: 30 * time.Second, instant: []promReply{ok}, wantStatus: core.GatePending, wantMessage: "2/5 samples passed", wantQueries: 3, wantFields: map[string]any{"last_error": nil}},
			},
		},
		{
			name: "a bad query stays pending until the timeout",
			gate: promGate(),
			steps: []promStep{
				{instant: []promReply{apiError(http.StatusBadRequest, "bad_data", "1:5: parse error")}, wantStatus: core.GatePending, wantMessage: "query failed: prometheus responded 400 Bad Request: bad_data: 1:5: parse error", wantQueries: 1},
				{advance: time.Minute, instant: []promReply{apiError(http.StatusBadRequest, "bad_data", "1:5: parse error")}, wantStatus: core.GatePending, wantMessage: "parse error", wantQueries: 2},
			},
		},
		{
			name: "only the last ten samples are kept",
			gate: promGate(func(g *config.Gate) { g.SuccessThreshold = new(20) }),
			steps: func() []promStep {
				var steps []promStep
				for i := range 12 {
					steps = append(steps, promStep{advance: time.Minute, instant: []promReply{ok}, wantStatus: core.GatePending, wantQueries: i + 1})
				}
				steps[11].wantMessage = "12/20 samples passed"
				steps[11].wantFields = map[string]any{"consecutive_successes": 12}
				return steps
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, last := runPromSteps(t, tt.gate, promDeployment, tt.steps)
			var ev struct {
				Samples []json.RawMessage `json:"samples"`
			}
			if err := json.Unmarshal(last.Evidence, &ev); err != nil {
				t.Fatalf("decode evidence: %v", err)
			}
			if len(ev.Samples) > 10 {
				t.Errorf("evidence keeps %d samples, want at most 10", len(ev.Samples))
			}
		})
	}
}

func TestPrometheusGateQueriesAtNow(t *testing.T) {
	t.Parallel()

	clock := newClock()
	fake, client := newFakeProm(t)
	fake.script([]promReply{vector("0")}, nil)
	gate := buildPrometheusGate(t, client, clock, promGate())
	evaluateGate(t, gate, promDeployment(clock), nil)
	q := fake.lastQuery(t)
	if q.Get("query") != "sum(rate(errors[5m]))" || q.Get("time") != "2026-11-23T12:00:00Z" {
		t.Errorf("query form = %v", q)
	}
}

func TestPrometheusGateBaseline(t *testing.T) {
	t.Parallel()

	baseline := func(b config.Baseline, mutate ...func(*config.Gate)) config.Gate {
		return promGate(append([]func(*config.Gate){func(g *config.Gate) {
			g.Condition = ""
			g.Baseline = &b
			g.SuccessThreshold = new(2)
			g.FailureThreshold = new(2)
		}}, mutate...)...)
	}
	history := matrix([]string{"1", "2", "NaN", "3", "+Inf"})
	tests := []struct {
		name       string
		gate       config.Gate
		deployment func(*fakeClock) core.Deployment
		steps      []promStep
	}{
		{
			name: "mean of the window before the deploy with a ratio",
			gate: baseline(config.Baseline{Ratio: 0.5}),
			steps: []promStep{
				{ranges: []promReply{history}, instant: []promReply{vector("2.5")}, wantStatus: core.GatePending, wantMessage: "1/2 samples passed; latest 2.5 (baseline limit <= 3)", wantQueries: 1, wantRanges: 1, wantFields: map[string]any{
					"baseline":  "map[direction:increase from:2026-11-23T11:25:00Z limit:3 points:3 status:ok to:2026-11-23T11:55:00Z value:2]",
					"condition": nil,
				}},
				{advance: time.Minute, instant: []promReply{vector("3.5")}, wantStatus: core.GatePending, wantMessage: "1/2 samples failed; latest 3.5 (baseline limit <= 3)", wantQueries: 2, wantRanges: 1},
				{advance: time.Minute, instant: []promReply{vector("3")}, wantStatus: core.GatePending, wantQueries: 3, wantRanges: 1},
				{advance: time.Minute, instant: []promReply{vector("0")}, wantStatus: core.GatePassed, wantMessage: "2 consecutive samples passed", wantQueries: 4, wantRanges: 1},
			},
		},
		{
			name: "decrease with an absolute margin",
			gate: baseline(config.Baseline{Direction: config.BaselineDecrease, Absolute: 0.5}),
			steps: []promStep{
				{ranges: []promReply{history}, instant: []promReply{vector("1.4")}, wantStatus: core.GatePending, wantMessage: "1/2 samples failed; latest 1.4 (baseline limit >= 1.5)", wantQueries: 1, wantRanges: 1},
				{advance: time.Minute, instant: []promReply{vector("1.5")}, wantStatus: core.GatePending, wantMessage: "1/2 samples passed", wantQueries: 2, wantRanges: 1},
			},
		},
		{
			name: "zero baseline relies on the absolute margin",
			gate: baseline(config.Baseline{Ratio: 2, Absolute: 0.01}),
			steps: []promStep{
				{ranges: []promReply{matrix([]string{"0", "0"})}, instant: []promReply{vector("0.02")}, wantStatus: core.GatePending, wantMessage: "1/2 samples failed; latest 0.02 (baseline limit <= 0.01)", wantQueries: 1, wantRanges: 1},
			},
		},
		{
			name: "condition and baseline must both hold",
			gate: baseline(config.Baseline{Ratio: 10}, func(g *config.Gate) { g.Condition = "< 2" }),
			steps: []promStep{
				{ranges: []promReply{history}, instant: []promReply{vector("2.5")}, wantStatus: core.GatePending, wantMessage: "1/2 samples failed; latest 2.5 (want < 2; baseline limit <= 22)", wantQueries: 1, wantRanges: 1},
				{advance: time.Minute, instant: []promReply{vector("1")}, wantStatus: core.GatePending, wantMessage: "1/2 samples passed", wantQueries: 2, wantRanges: 1},
			},
		},
		{
			name: "missing baseline is ignored by default",
			gate: baseline(config.Baseline{Ratio: 0.5}, func(g *config.Gate) { g.Condition = "< 2" }),
			steps: []promStep{
				{ranges: []promReply{matrix()}, instant: []promReply{vector("1")}, wantStatus: core.GatePending, wantMessage: "1/2 samples passed; latest 1 (want < 2); baseline unavailable, compared against the condition only", wantQueries: 1, wantRanges: 1, wantFields: map[string]any{
					"baseline": "map[direction:increase from:2026-11-23T11:25:00Z points:0 status:missing to:2026-11-23T11:55:00Z]",
				}},
				{advance: time.Minute, instant: []promReply{vector("1")}, wantStatus: core.GatePassed, wantMessage: "baseline unavailable", wantQueries: 2, wantRanges: 1},
			},
		},
		{
			name: "missing baseline without a condition passes on data alone",
			gate: baseline(config.Baseline{Ratio: 0.5}),
			steps: []promStep{
				{ranges: []promReply{matrix([]string{"NaN"})}, instant: []promReply{vector("100")}, wantStatus: core.GatePending, wantMessage: "1/2 samples passed; latest 100; baseline unavailable, so any value passes", wantQueries: 1, wantRanges: 1},
			},
		},
		{
			name: "missing baseline fails when configured",
			gate: baseline(config.Baseline{Ratio: 0.5, NoData: config.BaselineNoDataFail}),
			steps: []promStep{
				{ranges: []promReply{matrix()}, wantStatus: core.GateFailed, wantMessage: "no baseline data", wantRanges: 1, wantFields: map[string]any{"baseline": "map[direction:increase from:2026-11-23T11:25:00Z points:0 status:missing to:2026-11-23T11:55:00Z]"}},
			},
		},
		{
			name: "baseline errors are retried after the interval",
			gate: baseline(config.Baseline{Ratio: 0.5}),
			steps: []promStep{
				{ranges: []promReply{apiError(http.StatusInternalServerError, "internal", "boom")}, wantStatus: core.GatePending, wantMessage: "baseline query failed: prometheus responded 500 Internal Server Error: internal: boom", wantRanges: 1, wantFields: map[string]any{"baseline": nil}},
				{advance: 30 * time.Second, wantStatus: core.GatePending, wantMessage: "baseline query failed", wantRanges: 1, wantSame: true},
				{advance: 30 * time.Second, ranges: []promReply{history}, instant: []promReply{vector("1")}, wantStatus: core.GatePending, wantMessage: "1/2 samples passed", wantQueries: 1, wantRanges: 2},
			},
		},
		{
			name: "baseline with several series is a query problem",
			gate: baseline(config.Baseline{Ratio: 0.5}),
			steps: []promStep{
				{ranges: []promReply{matrix([]string{"1"}, []string{"2"})}, wantStatus: core.GatePending, wantMessage: "baseline query failed: query returned 2 series; aggregate it to a single value", wantRanges: 1},
			},
		},
		{
			name: "baseline anchored at creation without a start time",
			gate: baseline(config.Baseline{Ratio: 0.5, Window: new(config.Duration(time.Hour))}),
			deployment: func(c *fakeClock) core.Deployment {
				d := promDeployment(c)
				d.StartedAt = nil
				return d
			},
			steps: []promStep{
				{ranges: []promReply{history}, instant: []promReply{vector("1")}, wantStatus: core.GatePending, wantQueries: 1, wantRanges: 1, wantFields: map[string]any{
					"baseline": "map[direction:increase from:2026-11-23T10:50:00Z limit:3 points:3 status:ok to:2026-11-23T11:50:00Z value:2]",
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dep := tt.deployment
			if dep == nil {
				dep = promDeployment
			}
			fake, _ := runPromSteps(t, tt.gate, dep, tt.steps)
			if _, ranges := fake.counts(); ranges > 0 {
				form := fake.lastRange(t)
				if form.Get("query") != "sum(rate(errors[5m]))" || form.Get("step") != "60" {
					t.Errorf("range form = %v", form)
				}
				if form.Get("end") != "2026-11-23T11:55:00Z" && form.Get("end") != "2026-11-23T11:50:00Z" {
					t.Errorf("range end = %s, want the deployment start", form.Get("end"))
				}
			}
		})
	}
}

func TestPrometheusGateStartsFresh(t *testing.T) {
	t.Parallel()

	clock := newClock()
	fake, client := newFakeProm(t)
	gate := buildPrometheusGate(t, client, clock, promGate())
	d := promDeployment(clock)
	recent := `{"server":"main","query":"sum(rate(errors[5m]))","started_at":"2026-11-23T11:00:00Z","last_attempt_at":"2026-11-23T12:00:00Z","consecutive_successes":4,"samples":[]}`
	tests := []struct {
		name     string
		previous *core.GateResult
	}{
		{name: "no previous result"},
		{name: "previous verdict", previous: &core.GateResult{Status: core.GatePassed, Message: "old", Evidence: json.RawMessage(recent)}},
		{name: "undecodable evidence", previous: &core.GateResult{Status: core.GatePending, Message: "old", Evidence: json.RawMessage(`{"started_at":5}`)}},
		{name: "different query", previous: &core.GateResult{Status: core.GatePending, Message: "old", Evidence: json.RawMessage(strings.Replace(recent, "errors", "other", 1))}},
		{name: "start in the future", previous: &core.GateResult{Status: core.GatePending, Message: "old", Evidence: json.RawMessage(strings.Replace(recent, "11:00:00", "13:00:00", 1))}},
	}
	for _, tt := range tests {
		fake.script([]promReply{vector("0")}, nil)
		before, _ := fake.counts()
		ev := evaluateGate(t, gate, d, tt.previous)
		after, _ := fake.counts()
		fields := evidenceMap(t, ev)
		if after != before+1 || ev.Message != "1/5 samples passed; latest 0 (want < 0.05)" || fields["started_at"] != "2026-11-23T12:00:00Z" {
			t.Errorf("%s: result = %s %s after %d requests", tt.name, ev.Message, ev.Evidence, after-before)
		}
	}

	pending := &core.GateResult{Status: core.GatePending, Message: "old", Evidence: json.RawMessage(strings.Replace(recent, "12:00:00", "11:58:00", 1))}
	fake.script([]promReply{vector("0")}, nil)
	ev := evaluateGate(t, gate, d, pending)
	if ev.Status != core.GatePassed || evidenceMap(t, ev)["started_at"] != "2026-11-23T11:00:00Z" {
		t.Errorf("carried state: %s %s %s", ev.Status, ev.Message, ev.Evidence)
	}

	future := &core.GateResult{Status: core.GatePending, Message: "old", Evidence: json.RawMessage(strings.Replace(recent, "last_attempt_at\":\"2026-11-23T12", "last_attempt_at\":\"2026-11-23T13", 1))}
	fake.script([]promReply{vector("0")}, nil)
	before, _ := fake.counts()
	evaluateGate(t, gate, d, future)
	if after, _ := fake.counts(); after != before+1 {
		t.Error("an attempt time in the future throttled the gate")
	}
}

func TestPrometheusGateContext(t *testing.T) {
	t.Parallel()

	t.Run("cancelled before the call", func(t *testing.T) {
		t.Parallel()
		clock := newClock()
		fake, client := newFakeProm(t)
		gate := buildPrometheusGate(t, client, clock, promGate())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := gate.Evaluate(ctx, gates.Request{Deployment: promDeployment(clock)}); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
		if q, r := fake.counts(); q+r != 0 {
			t.Error("a cancelled call reached Prometheus")
		}
	})

	for _, withBaseline := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled during the query, baseline %t", withBaseline), func(t *testing.T) {
			t.Parallel()
			clock := newClock()
			fake, client := newFakeProm(t)
			fake.block = make(chan struct{})
			t.Cleanup(func() { close(fake.block) })
			g := promGate()
			if withBaseline {
				g.Baseline = &config.Baseline{Ratio: 1}
			}
			gate := buildPrometheusGate(t, client, clock, g)
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			if _, err := gate.Evaluate(ctx, gates.Request{Deployment: promDeployment(clock)}); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error = %v, want context.DeadlineExceeded", err)
			}
		})
	}
}

type staticQuerier struct{}

func (staticQuerier) Query(context.Context, string, time.Time) (promquery.Result, error) {
	return promquery.Result{Type: promquery.ResultScalar, Series: []promquery.Series{{Points: []promquery.Point{{Value: 0}}}}}, nil
}

func (staticQuerier) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) (promquery.Result, error) {
	return promquery.Result{Type: promquery.ResultMatrix}, nil
}

func TestRegisterPrometheus(t *testing.T) {
	t.Parallel()

	if err := gates.RegisterPrometheus(nil, gates.Prometheus{}); err == nil {
		t.Error("RegisterPrometheus accepted a nil registry")
	}
	if err := gates.RegisterPrometheus(gates.NewRegistry(), gates.Prometheus{Servers: map[string]gates.MetricsQuerier{"main": nil}}); err == nil || !strings.Contains(err.Error(), `server "main" has no querier`) {
		t.Errorf("nil querier error = %v", err)
	}
	r := gates.NewRegistry()
	if err := gates.RegisterPrometheus(r, gates.Prometheus{}); err != nil {
		t.Fatalf("RegisterPrometheus without servers: %v", err)
	}
	if err := gates.RegisterPrometheus(r, gates.Prometheus{}); !errors.Is(err, gates.ErrDuplicateGateType) {
		t.Errorf("second RegisterPrometheus error = %v, want ErrDuplicateGateType", err)
	}
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
		t.Errorf("RegisterBuiltins after RegisterPrometheus: %v", err)
	}
}

const prometheusConfig = `
prometheus:
  - name: main
    url: http://prometheus:9090
apps:
  - name: web
    source: {repo: acme/web}
    image: {repository: ghcr.io/acme/web}
    gitops: {repo: acme/platform, path: apps/web}
    gates:
      after:
        - type: health
        - type: prometheus
          name: error-rate
          query: sum(rate(errors[5m]))
          condition: "< 0.05"
          initial_delay: 1m
`

func TestRegisterPrometheusPlans(t *testing.T) {
	t.Parallel()

	catalog := loadCatalog(t, prometheusConfig)
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	if err := gates.RegisterPrometheus(r, gates.Prometheus{Servers: map[string]gates.MetricsQuerier{"main": staticQuerier{}}}); err != nil {
		t.Fatalf("RegisterPrometheus: %v", err)
	}
	plans, err := r.Plans(catalog)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	b := plans["web"].After[1]
	if b.Name != "error-rate" || b.Type != config.GatePrometheus || b.Kind != metrics.GateKindMetrics || !b.SkipOnRollback || b.Timeout != 16*time.Minute || b.Phase != core.GatePhaseAfter {
		t.Errorf("binding = %+v", b)
	}

	missing := gates.NewRegistry()
	if err := gates.RegisterPrometheus(missing, gates.Prometheus{Servers: map[string]gates.MetricsQuerier{"other": staticQuerier{}}}); err != nil {
		t.Fatalf("RegisterPrometheus: %v", err)
	}
	catalog.Apps[0].Gates.After = catalog.Apps[0].Gates.After[1:]
	if _, err := missing.Plans(catalog); err == nil || !strings.Contains(err.Error(), `app web: gates.after error-rate: build prometheus gate: prometheus server "main" is not configured`) {
		t.Errorf("Plans error = %v", err)
	}

	unregistered := gates.NewRegistry()
	if _, err := unregistered.Plans(catalog); !errors.Is(err, gates.ErrUnknownGateType) {
		t.Errorf("Plans without RegisterPrometheus error = %v, want ErrUnknownGateType", err)
	}
}

func TestPrometheusGateSkippedOnRollback(t *testing.T) {
	t.Parallel()

	clock := newClock()
	fake, client := newFakeProm(t)
	r := gates.NewRegistry()
	if err := gates.RegisterPrometheus(r, gates.Prometheus{Servers: map[string]gates.MetricsQuerier{"main": client}, Now: clock.Now}); err != nil {
		t.Fatalf("RegisterPrometheus: %v", err)
	}
	plan, err := r.Plan(config.App{Name: "web", Gates: config.Gates{After: []config.Gate{promGate()}}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	h := newHarness(t, plan)
	out := mustEvaluate(t, h, deployment(core.KindRollback), core.GatePhaseAfter)
	if out.Status != core.GatePassed || out.Results[0].Status != core.GateSkipped || out.Results[0].Message != "skipped for rollback" {
		t.Errorf("outcome = %+v", out)
	}
	if q, rq := fake.counts(); q+rq != 0 {
		t.Error("a rollback queried Prometheus")
	}

	fake.script([]promReply{vector("0")}, nil)
	deploy := deployment(core.KindDeploy)
	deploy.ID = 8
	out = mustEvaluate(t, h, deploy, core.GatePhaseAfter)
	if out.Results[0].Status != core.GatePending || !strings.Contains(out.Results[0].Message, "1/5 samples passed") {
		t.Errorf("deploy outcome = %+v", out.Results[0])
	}
}
