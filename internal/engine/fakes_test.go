package engine_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/engine"
)

const (
	appSHA     = "0123456789abcdef0123456789abcdef01234567"
	pinSHA     = "89abcdef0123456789abcdef0123456789abcdef"
	repinSHA   = "fedcba9876543210fedcba9876543210fedcba98"
	argoSHA    = "1111111111111111111111111111111111111111"
	gitopsRepo = "example-user/homelab"

	digestNew = core.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222")
	digestOld = core.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	digestBad = core.Digest("sha256:3333333333333333333333333333333333333333333333333333333333333333")

	syncTimeout = 10 * time.Minute
)

var (
	target = core.AppTarget("blog")
	epoch  = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type enteredKey struct {
	id    core.DeploymentID
	state core.State
}

type fakeStore struct {
	clock *clock

	mu          sync.Mutex
	nextID      core.DeploymentID
	deployments map[core.DeploymentID]core.Deployment
	entered     map[enteredKey]time.Time
	transitions []engine.Transition
	lineage     []core.Lineage
	failGet     error
}

func newFakeStore(c *clock) *fakeStore {
	return &fakeStore{
		clock:       c,
		deployments: map[core.DeploymentID]core.Deployment{},
		entered:     map[enteredKey]time.Time{},
	}
}

func (s *fakeStore) add(d core.Deployment) core.Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	if d.ID == 0 {
		d.ID = s.nextID + 100
	}
	if d.State == "" {
		d.State = core.StateQueued
	}
	now := s.clock.Now()
	d.CreatedAt = now
	if d.State.Active() || d.State.Terminal() {
		d.StartedAt = &now
	}
	s.deployments[d.ID] = d
	s.entered[enteredKey{d.ID, d.State}] = now
	return d
}

func (s *fakeStore) setState(id core.DeploymentID, state core.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deployments[id]
	d.State = state
	if state.Terminal() {
		now := s.clock.Now()
		d.FinishedAt = &now
	}
	s.deployments[id] = d
}

func (s *fakeStore) deployment(id core.DeploymentID) core.Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deployments[id]
}

func (s *fakeStore) recorded() []engine.Transition {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]engine.Transition(nil), s.transitions...)
}

func (s *fakeStore) states() []core.State {
	var states []core.State
	for _, tr := range s.recorded() {
		states = append(states, tr.To)
	}
	return states
}

func (s *fakeStore) Get(_ context.Context, id core.DeploymentID) (core.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGet != nil {
		return core.Deployment{}, s.failGet
	}
	d, ok := s.deployments[id]
	if !ok {
		return core.Deployment{}, fmt.Errorf("deployment %d: %w", id, core.ErrNotFound)
	}
	return d, nil
}

func (s *fakeStore) Transition(_ context.Context, t engine.Transition) (core.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deployments[t.ID]
	switch {
	case !ok:
		return core.Deployment{}, core.ErrNotFound
	case d.State != t.From:
		return core.Deployment{}, fmt.Errorf("%w: deployment %d is %s, expected %s", core.ErrStateConflict, t.ID, d.State, t.From)
	}
	if err := t.From.Transition(t.To); err != nil {
		return core.Deployment{}, err
	}
	if (t.To == core.StateFailed) != (t.FailureReason != "") {
		return core.Deployment{}, errors.New("failure reason only on failed transitions")
	}
	if err := t.Lineage.Validate(); err != nil {
		return core.Deployment{}, err
	}
	now := s.clock.Now()
	d.State = t.To
	applyLineage(&d, t.Lineage)
	if t.To.Active() && d.StartedAt == nil {
		d.StartedAt = &now
	}
	if t.To.Terminal() {
		d.FinishedAt = &now
	}
	if t.FailureReason != "" {
		d.FailureReason = t.FailureReason
	}
	s.deployments[t.ID] = d
	s.entered[enteredKey{t.ID, t.To}] = now
	s.transitions = append(s.transitions, t)
	return d, nil
}

func (s *fakeStore) SetLineage(_ context.Context, id core.DeploymentID, l core.Lineage, _ string) (core.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deployments[id]
	switch {
	case !ok:
		return core.Deployment{}, core.ErrNotFound
	case d.State.Terminal():
		return core.Deployment{}, core.ErrStateConflict
	case l.IsZero():
		return core.Deployment{}, errors.New("empty lineage")
	}
	applyLineage(&d, l)
	s.deployments[id] = d
	s.lineage = append(s.lineage, l)
	return d, nil
}

func (s *fakeStore) TimeInState(_ context.Context, id core.DeploymentID, state core.State) (time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.entered[enteredKey{id, state}]
	if !ok {
		return 0, core.ErrNotFound
	}
	return s.clock.Now().Sub(at), nil
}

func applyLineage(d *core.Deployment, l core.Lineage) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	if l.Digest != "" {
		d.Digest = l.Digest
	}
	if l.PreviousDigest != "" {
		d.PreviousDigest = l.PreviousDigest
	}
	set(&d.GitOpsRepository, l.GitOpsRepository)
	set(&d.PinCommitSHA, l.PinCommitSHA)
	set(&d.ArgoCDRevision, l.ArgoCDRevision)
}

type fakeResolver struct {
	mu    sync.Mutex
	refs  []core.ImageRef
	steps []func() (core.Digest, error)
}

func (r *fakeResolver) Resolve(_ context.Context, ref core.ImageRef) (core.Digest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	if len(r.steps) == 0 {
		return digestNew, nil
	}
	next := r.steps[0]
	if len(r.steps) > 1 {
		r.steps = r.steps[1:]
	}
	return next()
}

func (r *fakeResolver) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.refs)
}

type fakePins struct {
	mu       sync.Mutex
	pins     []engine.PinRequest
	abandons []engine.PinRequest
	pinFn    func(ctx context.Context, n int, req engine.PinRequest) (engine.PinResult, error)
}

func (p *fakePins) Pin(ctx context.Context, req engine.PinRequest) (engine.PinResult, error) {
	p.mu.Lock()
	p.pins = append(p.pins, req)
	n := len(p.pins)
	fn := p.pinFn
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, n, req)
	}
	return engine.PinResult{Commit: core.Commit{Repository: gitopsRepo, SHA: pinSHA}, Previous: digestOld, Changed: !req.Repin}, nil
}

func (p *fakePins) Abandon(_ context.Context, req engine.PinRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.abandons = append(p.abandons, req)
	return nil
}

func (p *fakePins) pinRequests() []engine.PinRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]engine.PinRequest(nil), p.pins...)
}

func (p *fakePins) abandoned() []engine.PinRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]engine.PinRequest(nil), p.abandons...)
}

type waitCall struct {
	revision string
	timeout  time.Duration
}

type fakeSync struct {
	mu      sync.Mutex
	waits   []waitCall
	waitFn  func(ctx context.Context, revision string) (core.SyncStatus, error)
	checkFn func(revision string) (core.SyncStatus, error)
	checked []string
	health  []core.Health
	healthE error
	checks  int
}

func (s *fakeSync) WaitSynced(ctx context.Context, _ core.Target, revision string) (core.SyncStatus, error) {
	call := waitCall{revision: revision}
	if deadline, ok := ctx.Deadline(); ok {
		call.timeout = time.Until(deadline)
	}
	s.mu.Lock()
	s.waits = append(s.waits, call)
	fn := s.waitFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, revision)
	}
	return syncedStatus(), nil
}

func (s *fakeSync) Check(_ context.Context, _ core.Target, revision string) (core.SyncStatus, error) {
	s.mu.Lock()
	s.checked = append(s.checked, revision)
	fn := s.checkFn
	s.mu.Unlock()
	if fn != nil {
		return fn(revision)
	}
	return syncedStatus(), nil
}

func syncedStatus() core.SyncStatus {
	return core.SyncStatus{State: core.SyncSynced, Revision: argoSHA, Reached: true, Health: core.Health{Status: core.HealthHealthy}}
}

func (s *fakeSync) Health(context.Context, core.Target) (core.Health, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	if s.healthE != nil {
		return core.Health{}, s.healthE
	}
	if len(s.health) == 0 {
		return core.Health{Status: core.HealthHealthy}, nil
	}
	next := s.health[0]
	if len(s.health) > 1 {
		s.health = s.health[1:]
	}
	return next, nil
}

func (s *fakeSync) waitCalls() []waitCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]waitCall(nil), s.waits...)
}

type fakeGates struct {
	mu          sync.Mutex
	phases      []core.GatePhase
	outcomes    map[core.GatePhase][]core.GateOutcome
	err         error
	reevaluated []core.Deployment
	rerun       *core.GateOutcome
	reErr       error
}

func (g *fakeGates) Reevaluate(_ context.Context, d core.Deployment, phase core.GatePhase) (core.GateOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reevaluated = append(g.reevaluated, d)
	if g.reErr != nil {
		return core.GateOutcome{}, g.reErr
	}
	outcome := core.GateOutcome{Status: core.GatePassed}
	if g.rerun != nil {
		outcome = *g.rerun
	}
	outcome.Phase = phase
	if g.outcomes == nil {
		g.outcomes = map[core.GatePhase][]core.GateOutcome{}
	}
	g.outcomes[phase] = []core.GateOutcome{outcome}
	return outcome, nil
}

func (g *fakeGates) reevaluations() []core.Deployment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]core.Deployment(nil), g.reevaluated...)
}

func (g *fakeGates) Evaluate(_ context.Context, _ core.Deployment, phase core.GatePhase) (core.GateOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.phases = append(g.phases, phase)
	if g.err != nil {
		return core.GateOutcome{}, g.err
	}
	queue := g.outcomes[phase]
	if len(queue) == 0 {
		return core.GateOutcome{Phase: phase, Status: core.GatePassed}, nil
	}
	next := queue[0]
	if len(queue) > 1 {
		g.outcomes[phase] = queue[1:]
	}
	next.Phase = phase
	return next, nil
}

func (g *fakeGates) set(phase core.GatePhase, outcomes ...core.GateOutcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.outcomes == nil {
		g.outcomes = map[core.GatePhase][]core.GateOutcome{}
	}
	g.outcomes[phase] = outcomes
}

func (g *fakeGates) evaluated() []core.GatePhase {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]core.GatePhase(nil), g.phases...)
}

type mirrorCall struct {
	trailers core.BlockTrailers
	commit   core.Commit
}

type fakeBlocks struct {
	mu         sync.Mutex
	blocked    *core.Block
	blockedErr error
	checked    int
	trailers   core.BlockTrailers
	trailerErr error
	mirrored   []mirrorCall
	markErr    error
}

func (b *fakeBlocks) Blocked(context.Context, core.Target, core.Commit, core.Digest) (core.Block, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checked++
	if b.blockedErr != nil {
		return core.Block{}, false, b.blockedErr
	}
	if b.blocked == nil {
		return core.Block{}, false, nil
	}
	return *b.blocked, true, nil
}

func (b *fakeBlocks) PendingTrailers(context.Context, core.Target) (core.BlockTrailers, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trailers, b.trailerErr
}

func (b *fakeBlocks) MarkMirrored(_ context.Context, _ core.Target, trailers core.BlockTrailers, commit core.Commit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mirrored = append(b.mirrored, mirrorCall{trailers: trailers, commit: commit})
	return b.markErr
}

func (b *fakeBlocks) checks() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checked
}

type fakeNotifier struct {
	mu    sync.Mutex
	notes []core.Notification
}

func (n *fakeNotifier) Notify(_ context.Context, note core.Notification) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := note.Validate(); err != nil {
		panic(fmt.Sprintf("invalid notification %s: %v", note.Kind, err))
	}
	n.notes = append(n.notes, note)
}

func (n *fakeNotifier) kinds() []core.NotificationKind {
	n.mu.Lock()
	defer n.mu.Unlock()
	var kinds []core.NotificationKind
	for _, note := range n.notes {
		kinds = append(kinds, note.Kind)
	}
	return kinds
}

func (n *fakeNotifier) all() []core.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]core.Notification(nil), n.notes...)
}

type finished struct {
	app      string
	outcome  engine.Outcome
	duration time.Duration
}

type fakeMetrics struct {
	mu       sync.Mutex
	started  []string
	finished []finished
}

func (m *fakeMetrics) DeploymentStarted(app string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, app)
}

func (m *fakeMetrics) DeploymentFinished(app string, outcome engine.Outcome, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished = append(m.finished, finished{app: app, outcome: outcome, duration: duration})
}

func (m *fakeMetrics) outcomes() []engine.Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []engine.Outcome
	for _, f := range m.finished {
		out = append(out, f.outcome)
	}
	return out
}

func (m *fakeMetrics) startedApps() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.started...)
}

type fakePolicy struct {
	mu       sync.Mutex
	failures []engine.Failure
	action   engine.FailureAction
	err      error
}

func (p *fakePolicy) Decide(_ context.Context, f engine.Failure) (engine.FailureAction, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = append(p.failures, f)
	if p.action == "" && p.err == nil {
		return engine.FailureActionFail, nil
	}
	return p.action, p.err
}

func (p *fakePolicy) decided() []engine.Failure {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]engine.Failure(nil), p.failures...)
}

type harness struct {
	clock    *clock
	store    *fakeStore
	resolver *fakeResolver
	pins     *fakePins
	sync     *fakeSync
	gates    *fakeGates
	blocks   *fakeBlocks
	notifier *fakeNotifier
	metrics  *fakeMetrics
	policy   *fakePolicy
	apps     map[core.Target]time.Duration
	options  engine.Options
}

func newHarness() *harness {
	c := &clock{now: epoch}
	return &harness{
		clock:    c,
		store:    newFakeStore(c),
		resolver: &fakeResolver{},
		pins:     &fakePins{},
		sync:     &fakeSync{},
		gates:    &fakeGates{},
		blocks:   &fakeBlocks{},
		notifier: &fakeNotifier{},
		metrics:  &fakeMetrics{},
		policy:   &fakePolicy{},
		apps:     map[core.Target]time.Duration{target: syncTimeout},
	}
}

func (h *harness) engine(t *testing.T) *engine.Engine {
	t.Helper()
	opts := h.options
	opts.Store = h.store
	opts.Resolver = h.resolver
	opts.Pins = h.pins
	opts.Sync = h.sync
	opts.Gates = h.gates
	opts.Blocks = h.blocks
	opts.Notifier = h.notifier
	opts.Metrics = h.metrics
	opts.Policy = h.policy
	apps := maps.Clone(h.apps)
	opts.SyncTimeout = func(t core.Target) (time.Duration, bool) {
		d, ok := apps[t]
		return d, ok
	}
	opts.Now = h.clock.Now
	e, err := engine.New(opts)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e
}

func (h *harness) run(t *testing.T, ctx context.Context, id core.DeploymentID, superseded ...core.DeploymentID) (engine.Result, error) {
	t.Helper()
	return h.engine(t).Handle(ctx, engine.Claim{Deployment: h.store.deployment(id), Attempt: 1, Superseded: superseded})
}

func deploy() core.Deployment {
	return core.Deployment{
		Target:      target,
		Kind:        core.KindDeploy,
		Source:      core.SourceWebhook,
		RequestedBy: "octocat",
		Commit:      core.Commit{Repository: "example-user/blog", SHA: appSHA},
		Image:       core.ImageRef{Repository: "ghcr.io/example-user/blog", Tag: appSHA},
	}
}

func in(state core.State, d core.Deployment) core.Deployment {
	d.State = state
	if state != core.StateQueued && state != core.StateResolving && d.Digest == "" {
		d.Digest = digestNew
	}
	if state == core.StateSyncing || state == core.StateVerifying {
		d.PinCommitSHA = pinSHA
		d.GitOpsRepository = gitopsRepo
	}
	return d
}

func failedGate(name, message string) core.GateOutcome {
	return core.GateOutcome{Status: core.GateFailed, Results: []core.GateResult{
		{Name: "ok", Status: core.GatePassed},
		{Name: name, Status: core.GateFailed, Message: message},
	}}
}

func pending() core.GateOutcome {
	return core.GateOutcome{Status: core.GatePending}
}
