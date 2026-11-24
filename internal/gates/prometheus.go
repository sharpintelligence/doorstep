package gates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/promquery"
)

const (
	maxPrometheusSamples = 10
	clockSkewTolerance   = 5 * time.Second
)

const (
	sampleSuccess = "success"
	sampleFailure = "failure"
	sampleNoData  = "no_data"

	baselineOK      = "ok"
	baselineMissing = "missing"
)

type MetricsQuerier interface {
	Query(ctx context.Context, expr string, at time.Time) (promquery.Result, error)
	QueryRange(ctx context.Context, expr string, from, to time.Time, step time.Duration) (promquery.Result, error)
}

type Prometheus struct {
	Servers map[string]MetricsQuerier
	Now     func() time.Time
}

func RegisterPrometheus(r *Registry, deps Prometheus) error {
	if r == nil {
		return errors.New("register prometheus gate: no registry")
	}
	for name, q := range deps.Servers {
		if q == nil {
			return fmt.Errorf("register prometheus gate: server %q has no querier", name)
		}
	}
	deps.Servers = maps.Clone(deps.Servers)
	if deps.Now == nil {
		deps.Now = time.Now
	}
	def := Definition{Kind: metrics.GateKindMetrics, SkipOnRollback: true, New: deps.newPrometheus}
	if err := r.Register(config.GatePrometheus, def); err != nil {
		return fmt.Errorf("register prometheus gate: %w", err)
	}
	return nil
}

func (p Prometheus) newPrometheus(_ config.App, gate config.Gate) (Gate, error) {
	spec, err := gate.PrometheusQuery()
	if err != nil {
		return nil, err
	}
	querier, ok := p.Servers[spec.Server]
	if !ok {
		return nil, fmt.Errorf("prometheus server %q is not configured", spec.Server)
	}
	sum := sha256.Sum256([]byte(spec.Query))
	return prometheusGate{spec: spec, queryHash: hex.EncodeToString(sum[:]), querier: querier, now: p.Now}, nil
}

type prometheusGate struct {
	spec      config.PrometheusQuery
	queryHash string
	querier   MetricsQuerier
	now       func() time.Time
}

type prometheusEvidence struct {
	Server               string            `json:"server"`
	Query                string            `json:"query"`
	QuerySHA256          string            `json:"query_sha256"`
	Condition            string            `json:"condition,omitempty"`
	Interval             string            `json:"interval"`
	SuccessThreshold     int               `json:"success_threshold"`
	FailureThreshold     int               `json:"failure_threshold"`
	StartedAt            string            `json:"started_at"`
	LastAttemptAt        string            `json:"last_attempt_at,omitempty"`
	ConsecutiveSuccesses int               `json:"consecutive_successes"`
	ConsecutiveFailures  int               `json:"consecutive_failures"`
	ConsecutiveNoData    int               `json:"consecutive_no_data"`
	LastError            string            `json:"last_error,omitempty"`
	Baseline             *baselineEvidence `json:"baseline,omitempty"`
	Samples              []sampleEvidence  `json:"samples"`
}

type baselineEvidence struct {
	Status    string `json:"status"`
	Direction string `json:"direction"`
	Value     string `json:"value,omitempty"`
	Limit     string `json:"limit,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	Points    int    `json:"points"`
}

type sampleEvidence struct {
	At      string `json:"at"`
	Value   string `json:"value,omitempty"`
	Outcome string `json:"outcome"`
}

type baselineResult struct {
	evidence *baselineEvidence
	limit    float64
	value    float64
}

type sample struct {
	outcome string
	value   float64
}

func (g prometheusGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return Evaluation{}, err
	}
	now := g.now()
	ev := pendingState[prometheusEvidence](req.Previous)
	started, ok := parseTimestamp(ev.StartedAt)
	if !ok || started.After(now.Add(clockSkewTolerance)) || ev.Server != g.spec.Server || ev.QuerySHA256 != g.queryHash {
		ev = prometheusEvidence{StartedAt: timestamp(now)}
		started = now
	}
	g.describe(&ev)

	if first := started.Add(g.spec.InitialDelay); now.Before(first) {
		return g.result(core.GatePending, "first sample at "+timestamp(first), ev)
	}
	if last, ok := parseTimestamp(ev.LastAttemptAt); ok && req.Previous != nil && g.throttled(now.Sub(last)) {
		return Evaluation{Status: core.GatePending, Message: req.Previous.Message, Evidence: req.Previous.Evidence}, nil
	}
	ev.LastAttemptAt = timestamp(now)
	ev.LastError = ""

	baseline, err := g.baseline(ctx, req.Deployment, &ev)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Evaluation{}, ctxErr
		}
		ev.LastError = clean("baseline query failed: " + err.Error())
		return g.result(queryErrorStatus(err), ev.LastError, ev)
	}
	if ev.Baseline != nil && ev.Baseline.Status == baselineMissing && g.spec.Baseline.NoData == config.BaselineNoDataFail {
		return g.result(core.GateFailed, "no baseline data", ev)
	}

	s, err := g.sample(ctx, now, baseline)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Evaluation{}, ctxErr
		}
		ev.LastError = clean("query failed: " + err.Error())
		return g.result(queryErrorStatus(err), g.withBaselineNote(ev.LastError, ev), ev)
	}
	g.record(&ev, now, s)
	status, message := g.verdict(ev, s, baseline)
	return g.result(status, g.withBaselineNote(message, ev), ev)
}

func (g prometheusGate) throttled(sinceLast time.Duration) bool {
	return sinceLast > -clockSkewTolerance && sinceLast < g.spec.Interval
}

func queryErrorStatus(err error) core.GateStatus {
	if rejectedQuery(err) {
		return core.GateFailed
	}
	return core.GatePending
}

func rejectedQuery(err error) bool {
	var promErr *promquery.Error
	if !errors.As(err, &promErr) || promErr.Type != promquery.ErrorBadData {
		return false
	}
	return promErr.Status == http.StatusBadRequest || promErr.Status == http.StatusUnprocessableEntity
}

func (g prometheusGate) describe(ev *prometheusEvidence) {
	ev.Server = g.spec.Server
	ev.Query = clean(g.spec.Query)
	ev.QuerySHA256 = g.queryHash
	ev.Condition = ""
	if g.spec.Condition != nil {
		ev.Condition = g.spec.Condition.String()
	}
	ev.Interval = g.spec.Interval.String()
	ev.SuccessThreshold = g.spec.SuccessThreshold
	ev.FailureThreshold = g.spec.FailureThreshold
	if ev.Samples == nil {
		ev.Samples = []sampleEvidence{}
	}
}

func (g prometheusGate) result(status core.GateStatus, message string, ev prometheusEvidence) (Evaluation, error) {
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}

func (g prometheusGate) baseline(ctx context.Context, d core.Deployment, ev *prometheusEvidence) (*baselineResult, error) {
	b := g.spec.Baseline
	if b == nil {
		ev.Baseline = nil
		return nil, nil
	}
	if ev.Baseline != nil {
		return restoreBaseline(ev.Baseline)
	}
	anchor := d.CreatedAt
	if d.StartedAt != nil && !d.StartedAt.IsZero() {
		anchor = *d.StartedAt
	}
	from := anchor.Add(-b.Window)
	res, err := g.querier.QueryRange(ctx, g.spec.Query, from, anchor, g.spec.Interval)
	if err != nil {
		return nil, err
	}
	if res.Type != promquery.ResultMatrix {
		return nil, fmt.Errorf("query returned a %s result; want a range vector", res.Type)
	}
	if len(res.Series) > 1 {
		return nil, fmt.Errorf("query returned %d series; aggregate it to a single value", len(res.Series))
	}
	be := &baselineEvidence{Direction: string(b.Direction), From: timestamp(from), To: timestamp(anchor)}
	sum := 0.0
	for _, s := range res.Series {
		for _, p := range s.Points {
			if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
				continue
			}
			sum += p.Value
			be.Points++
		}
	}
	ev.Baseline = be
	if be.Points == 0 {
		be.Status = baselineMissing
		return nil, nil
	}
	mean := sum / float64(be.Points)
	limit := b.Bound(mean)
	be.Status = baselineOK
	be.Value = formatValue(mean)
	be.Limit = formatValue(limit)
	return &baselineResult{evidence: be, value: mean, limit: limit}, nil
}

func restoreBaseline(be *baselineEvidence) (*baselineResult, error) {
	if be.Status != baselineOK {
		return nil, nil
	}
	value, valueErr := strconv.ParseFloat(be.Value, 64)
	limit, limitErr := strconv.ParseFloat(be.Limit, 64)
	if err := errors.Join(valueErr, limitErr); err != nil {
		return nil, fmt.Errorf("decode recorded baseline: %w", err)
	}
	return &baselineResult{evidence: be, value: value, limit: limit}, nil
}

func (g prometheusGate) sample(ctx context.Context, now time.Time, baseline *baselineResult) (sample, error) {
	res, err := g.querier.Query(ctx, g.spec.Query, now)
	if err != nil {
		return sample{}, err
	}
	if res.Type != promquery.ResultScalar && res.Type != promquery.ResultVector {
		return sample{}, fmt.Errorf("query returned a %s result; want a scalar or an instant vector", res.Type)
	}
	if len(res.Series) > 1 {
		return sample{}, fmt.Errorf("query returned %d series; aggregate it to a single value", len(res.Series))
	}
	if len(res.Series) == 0 || len(res.Series[0].Points) == 0 || math.IsNaN(res.Series[0].Points[0].Value) {
		return sample{outcome: sampleNoData, value: math.NaN()}, nil
	}
	v := res.Series[0].Points[0].Value
	ok := g.spec.Condition == nil || g.spec.Condition.Holds(v)
	if baseline != nil {
		ok = ok && g.spec.Baseline.Within(v, baseline.value)
	}
	if ok {
		return sample{outcome: sampleSuccess, value: v}, nil
	}
	return sample{outcome: sampleFailure, value: v}, nil
}

func (g prometheusGate) record(ev *prometheusEvidence, now time.Time, s sample) {
	switch s.outcome {
	case sampleSuccess:
		ev.ConsecutiveSuccesses++
		ev.ConsecutiveFailures = 0
		ev.ConsecutiveNoData = 0
	case sampleFailure:
		ev.ConsecutiveFailures++
		ev.ConsecutiveSuccesses = 0
		ev.ConsecutiveNoData = 0
	case sampleNoData:
		ev.ConsecutiveNoData++
		if g.spec.NoData == config.NoDataFail {
			ev.ConsecutiveFailures++
			ev.ConsecutiveSuccesses = 0
		}
	}
	entry := sampleEvidence{At: timestamp(now), Outcome: s.outcome}
	if s.outcome != sampleNoData {
		entry.Value = formatValue(s.value)
	}
	ev.Samples = append(ev.Samples, entry)
	if extra := len(ev.Samples) - maxPrometheusSamples; extra > 0 {
		ev.Samples = append([]sampleEvidence(nil), ev.Samples[extra:]...)
	}
}

func (g prometheusGate) verdict(ev prometheusEvidence, s sample, baseline *baselineResult) (core.GateStatus, string) {
	latest := "latest " + formatShort(s.value)
	if s.outcome == sampleNoData {
		latest = "latest had no data"
	}
	want := g.want(baseline)
	switch {
	case ev.ConsecutiveFailures >= g.spec.FailureThreshold:
		if ev.ConsecutiveNoData >= ev.ConsecutiveFailures {
			return core.GateFailed, "no data for " + samples(ev.ConsecutiveNoData)
		}
		return core.GateFailed, fmt.Sprintf("%s failed; %s%s", samples(ev.ConsecutiveFailures), latest, want)
	case ev.ConsecutiveSuccesses >= g.spec.SuccessThreshold:
		return core.GatePassed, fmt.Sprintf("%s passed; %s", samples(ev.ConsecutiveSuccesses), latest)
	case s.outcome == sampleNoData && g.spec.NoData == config.NoDataPending:
		return core.GatePending, "no data for " + samples(ev.ConsecutiveNoData) + "; waiting for data"
	case ev.ConsecutiveFailures > 0:
		return core.GatePending, fmt.Sprintf("%d/%d samples failed; %s%s", ev.ConsecutiveFailures, g.spec.FailureThreshold, latest, want)
	default:
		return core.GatePending, fmt.Sprintf("%d/%d samples passed; %s%s", ev.ConsecutiveSuccesses, g.spec.SuccessThreshold, latest, want)
	}
}

func (g prometheusGate) want(baseline *baselineResult) string {
	var parts []string
	if g.spec.Condition != nil {
		parts = append(parts, "want "+g.spec.Condition.String())
	}
	if baseline != nil {
		op := "<="
		if g.spec.Baseline.Direction == config.BaselineDecrease {
			op = ">="
		}
		parts = append(parts, fmt.Sprintf("baseline limit %s %s", op, formatShort(baseline.limit)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func (g prometheusGate) withBaselineNote(message string, ev prometheusEvidence) string {
	switch {
	case ev.Baseline == nil || ev.Baseline.Status != baselineMissing:
		return message
	case g.spec.Condition == nil:
		return message + "; baseline unavailable, so any value passes"
	default:
		return message + "; baseline unavailable, compared against the condition only"
	}
}

func samples(n int) string {
	if n == 1 {
		return "1 sample"
	}
	return strconv.Itoa(n) + " consecutive samples"
}

func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func formatShort(v float64) string {
	return strconv.FormatFloat(v, 'g', 4, 64)
}
