package gates_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

type gateFunc func(ctx context.Context, req gates.Request) (gates.Evaluation, error)

func (f gateFunc) Evaluate(ctx context.Context, req gates.Request) (gates.Evaluation, error) {
	return f(ctx, req)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeStore struct {
	mu        sync.Mutex
	clock     *fakeClock
	rows      map[string]core.GateResult
	order     []string
	records   []core.GateResult
	recordErr error
	listErr   error
	refuse    func(core.GateResult) error
}

func newStore(clock *fakeClock) *fakeStore {
	return &fakeStore{clock: clock, rows: map[string]core.GateResult{}}
}

func key(id core.DeploymentID, name string) string {
	return fmt.Sprintf("%d/%s", id, name)
}

func (s *fakeStore) RecordGateResult(_ context.Context, r core.GateResult) (core.GateResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return core.GateResult{}, false, s.recordErr
	}
	if !r.Phase.Valid() || !r.Status.Valid() || r.Name == "" || r.Type == "" {
		return core.GateResult{}, false, fmt.Errorf("fake store: invalid result %+v", r)
	}
	if s.refuse != nil {
		if err := s.refuse(r); err != nil {
			return core.GateResult{}, false, err
		}
	}
	if len(r.Evidence) == 0 {
		r.Evidence = json.RawMessage(`{}`)
	}
	now := s.clock.Now()
	k := key(r.DeploymentID, r.Name)
	prev, found := s.rows[k]
	r.UpdatedAt = now
	switch {
	case !found:
		r.StartedAt = now
		if r.Status != core.GatePending {
			r.FinishedAt = &now
		}
		s.order = append(s.order, k)
	default:
		r.StartedAt = prev.StartedAt
		if prev.Status != core.GatePending && r.Status == core.GatePending {
			r.StartedAt = now
		}
		switch {
		case r.Status == core.GatePending:
			r.FinishedAt = nil
		case prev.Status == r.Status:
			r.FinishedAt = prev.FinishedAt
		default:
			r.FinishedAt = &now
		}
	}
	s.rows[k] = r
	s.records = append(s.records, r)
	return r, !found || prev.Status != r.Status, nil
}

func (s *fakeStore) ListGateResults(_ context.Context, id core.DeploymentID) ([]core.GateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []core.GateResult
	for _, k := range s.order {
		if r := s.rows[k]; r.DeploymentID == id {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) get(id core.DeploymentID, name string) (core.GateResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[key(id, name)]
	return r, ok
}

func (s *fakeStore) recordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

type observation struct {
	app      string
	gate     metrics.GateKind
	phase    metrics.Phase
	result   metrics.GateResult
	duration time.Duration
}

type fakeMetrics struct {
	mu           sync.Mutex
	observations []observation
}

func (m *fakeMetrics) GateEvaluated(app string, gate metrics.GateKind, phase metrics.Phase, result metrics.GateResult, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observations = append(m.observations, observation{app, gate, phase, result, duration})
}

func (m *fakeMetrics) all() []observation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.observations)
}

type recordingObserver struct {
	mu      sync.Mutex
	results []core.GateResult
	err     error
}

func (o *recordingObserver) GateRecorded(_ context.Context, _ core.Deployment, r core.GateResult) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.results = append(o.results, r)
	return o.err
}

func (o *recordingObserver) statuses() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, 0, len(o.results))
	for _, r := range o.results {
		out = append(out, r.Name+"="+string(r.Status))
	}
	return out
}

type scriptedGate struct {
	mu       sync.Mutex
	verdicts []gates.Evaluation
	errs     []error
	requests []gates.Request
	before   func()
}

func verdicts(statuses ...core.GateStatus) *scriptedGate {
	g := &scriptedGate{}
	for _, s := range statuses {
		g.verdicts = append(g.verdicts, gates.Evaluation{Status: s, Message: string(s)})
	}
	return g
}

func (g *scriptedGate) Evaluate(_ context.Context, req gates.Request) (gates.Evaluation, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.before != nil {
		g.before()
	}
	i := len(g.requests)
	g.requests = append(g.requests, req)
	var err error
	if i < len(g.errs) {
		err = g.errs[i]
	}
	if len(g.verdicts) == 0 {
		return gates.Evaluation{}, err
	}
	return g.verdicts[min(i, len(g.verdicts)-1)], err
}

func (g *scriptedGate) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func (g *scriptedGate) lastRequest() gates.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[len(g.requests)-1]
}

func bind(name string, phase core.GatePhase, g gates.Gate) gates.Binding {
	return gates.Binding{Name: name, Type: config.GateType(name), Kind: metrics.GateKindArgoCD, Phase: phase, Gate: g}
}

type harness struct {
	clock    *fakeClock
	store    *fakeStore
	metrics  *fakeMetrics
	observer *recordingObserver
	logs     *syncBuffer
	eval     *gates.Evaluator
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T, plan gates.Plan, mutate ...func(*gates.Options)) *harness {
	t.Helper()
	h := &harness{clock: newClock(), metrics: &fakeMetrics{}, observer: &recordingObserver{}, logs: &syncBuffer{}}
	h.store = newStore(h.clock)
	opts := gates.Options{
		Store:     h.store,
		Plans:     map[string]gates.Plan{"web": plan},
		Metrics:   h.metrics,
		Observers: []gates.Observer{h.observer},
		Logger:    slog.New(slog.NewTextHandler(h.logs, nil)),
		Now:       h.clock.Now,
	}
	for _, m := range mutate {
		m(&opts)
	}
	eval, err := gates.NewEvaluator(opts)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	h.eval = eval
	return h
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func deployment(kind core.Kind) core.Deployment {
	d := core.Deployment{
		ID:     7,
		Target: core.Target{App: "web", Environment: "prod"},
		Kind:   kind,
		State:  core.StateVerifying,
		Commit: core.Commit{Repository: "acme/web", SHA: "0123456789abcdef0123456789abcdef01234567"},
	}
	if kind == core.KindRollback {
		of := core.DeploymentID(6)
		d.RollbackOfID = &of
	}
	return d
}

func mustEvaluate(t *testing.T, h *harness, d core.Deployment, phase core.GatePhase) core.GateOutcome {
	t.Helper()
	out, err := h.eval.Evaluate(t.Context(), d, phase)
	if err != nil {
		t.Fatalf("Evaluate(%s): %v", phase, err)
	}
	return out
}

func outcomeStatuses(out core.GateOutcome) []string {
	s := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		s = append(s, r.Name+"="+string(r.Status))
	}
	return s
}
