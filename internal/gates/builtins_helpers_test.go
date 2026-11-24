package gates_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	_ "time/tzdata"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

type fakeTracked struct {
	mu      sync.Mutex
	commits []core.TrackedCommit
	err     error
	calls   []string
}

func (f *fakeTracked) TrackedCommits(_ context.Context, repository, sha string) ([]core.TrackedCommit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repository+"@"+sha)
	return slices.Clone(f.commits), f.err
}

type fakeLive struct {
	mu       sync.Mutex
	eval     core.ChecksEvaluation
	err      error
	calls    int
	required []string
}

func (f *fakeLive) Checks(_ context.Context, _, _ string, required []string) (core.ChecksEvaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.required = slices.Clone(required)
	return f.eval, f.err
}

func (f *fakeLive) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeFreezes struct {
	mu     sync.Mutex
	locks  []core.Lock
	err    error
	target core.Target
}

func (f *fakeFreezes) Blocking(_ context.Context, t core.Target) ([]core.Lock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = t
	return slices.Clone(f.locks), f.err
}

type fakeApplications struct {
	mu        sync.Mutex
	app       gates.Application
	err       error
	namespace string
	name      string
}

func (f *fakeApplications) Application(_ context.Context, namespace, name string) (gates.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespace, f.name = namespace, name
	return f.app, f.err
}

func (f *fakeApplications) set(app gates.Application, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.app, f.err = app, err
}

type builtinFakes struct {
	clock   *fakeClock
	tracked *fakeTracked
	live    *fakeLive
	freezes *fakeFreezes
	apps    *fakeApplications
}

func newBuiltinFakes() *builtinFakes {
	return &builtinFakes{
		clock:   newClock(),
		tracked: &fakeTracked{},
		live:    &fakeLive{},
		freezes: &fakeFreezes{},
		apps:    &fakeApplications{},
	}
}

func (f *builtinFakes) deps() gates.Builtins {
	return gates.Builtins{
		Tracked:      f.tracked,
		Live:         f.live,
		Freezes:      f.freezes,
		Applications: f.apps,
		Now:          f.clock.Now,
	}
}

func webApp() config.App {
	return config.App{
		Name:   "web",
		Source: config.Source{Repo: "acme/web", RequiredChecks: []string{"test", "lint"}},
		ArgoCD: config.ArgoCD{Application: "web-prod", Namespace: "argocd"},
	}
}

func builtinGate(t *testing.T, deps gates.Builtins, app config.App, g config.Gate) gates.Gate {
	t.Helper()
	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, deps); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	if g.Name == "" {
		g.Name = string(g.Type)
	}
	app.Gates = config.Gates{Before: []config.Gate{g}, After: []config.Gate{}}
	plan, err := r.Plan(app)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan.Before[0].Gate
}

func evaluateGate(t *testing.T, g gates.Gate, d core.Deployment, previous *core.GateResult) gates.Evaluation {
	t.Helper()
	ev, err := g.Evaluate(t.Context(), gates.Request{Deployment: d, Phase: core.GatePhaseBefore, Previous: previous})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !ev.Status.Valid() {
		t.Fatalf("Evaluate returned status %q", ev.Status)
	}
	if err := core.ValidateGateEvidence(ev.Evidence); err != nil {
		t.Fatalf("Evaluate returned invalid evidence: %v", err)
	}
	return ev
}

func resultOf(ev gates.Evaluation) *core.GateResult {
	return &core.GateResult{Status: ev.Status, Message: ev.Message, Evidence: ev.Evidence}
}

func evidenceMap(t *testing.T, ev gates.Evaluation) map[string]any {
	t.Helper()
	if len(ev.Evidence) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(ev.Evidence, &m); err != nil {
		t.Fatalf("decode evidence %s: %v", ev.Evidence, err)
	}
	return m
}
