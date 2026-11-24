package gates_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

func TestRegisterBuiltinsValidatesDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*gates.Builtins)
		wantErr []string
	}{
		{name: "all dependencies", mutate: func(*gates.Builtins) {}},
		{name: "live checks are optional", mutate: func(b *gates.Builtins) { b.Live = nil }},
		{name: "clock is optional", mutate: func(b *gates.Builtins) { b.Now = nil }},
		{name: "no tracked commits", mutate: func(b *gates.Builtins) { b.Tracked = nil }, wantErr: []string{"checks gate needs tracked commits"}},
		{name: "no freeze reader", mutate: func(b *gates.Builtins) { b.Freezes = nil }, wantErr: []string{"lock gate needs a freeze reader"}},
		{name: "no application reader", mutate: func(b *gates.Builtins) { b.Applications = nil }, wantErr: []string{"health and soak gates need an Argo CD application reader"}},
		{name: "negative grace", mutate: func(b *gates.Builtins) { b.DegradedGrace = -time.Second }, wantErr: []string{"degraded grace -1s is negative"}},
		{
			name:    "every problem is reported",
			mutate:  func(b *gates.Builtins) { *b = gates.Builtins{} },
			wantErr: []string{"tracked commits", "freeze reader", "application reader"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps := newBuiltinFakes().deps()
			tt.mutate(&deps)
			err := gates.RegisterBuiltins(gates.NewRegistry(), deps)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("RegisterBuiltins: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RegisterBuiltins accepted invalid dependencies")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestRegisterBuiltinsRegistryErrors(t *testing.T) {
	t.Parallel()

	if err := gates.RegisterBuiltins(nil, newBuiltinFakes().deps()); err == nil {
		t.Error("RegisterBuiltins accepted a nil registry")
	}
	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); !errors.Is(err, gates.ErrDuplicateGateType) {
		t.Errorf("second RegisterBuiltins error = %v, want ErrDuplicateGateType", err)
	}
}

const allGatesConfig = `
apps:
  - name: web
    source: {repo: acme/web, required_checks: [test]}
    image: {repository: ghcr.io/acme/web}
    gitops: {repo: acme/platform, path: apps/web}
    argocd: {application: web-prod}
    gates:
      before:
        - type: checks
        - type: lock
        - type: schedule
          timezone: Europe/London
          windows:
            - {effect: deny, cron: "0 17 * * 5", duration: 63h, name: weekend}
      after:
        - type: health
          timeout: 15m
        - type: soak
          duration: 30m
`

func TestRegisterBuiltinsPlans(t *testing.T) {
	t.Parallel()

	catalog := loadCatalog(t, allGatesConfig)
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, newBuiltinFakes().deps()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	plans, err := r.Plans(catalog)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	plan := plans["web"]

	type want struct {
		name    string
		kind    metrics.GateKind
		skip    bool
		timeout time.Duration
		phase   core.GatePhase
	}
	wants := []want{
		{name: "checks", kind: metrics.GateKindCI, skip: true, timeout: 30 * time.Minute, phase: core.GatePhaseBefore},
		{name: "lock", kind: metrics.GateKindLock, skip: true, phase: core.GatePhaseBefore},
		{name: "schedule", kind: metrics.GateKindSchedule, phase: core.GatePhaseBefore},
		{name: "health", kind: metrics.GateKindArgoCD, timeout: 15 * time.Minute, phase: core.GatePhaseAfter},
		{name: "soak", kind: metrics.GateKindSoak, skip: true, timeout: 40 * time.Minute, phase: core.GatePhaseAfter},
	}
	bindings := append(append([]gates.Binding{}, plan.Before...), plan.After...)
	if len(bindings) != len(wants) {
		t.Fatalf("plan has %d bindings, want %d", len(bindings), len(wants))
	}
	for i, w := range wants {
		b := bindings[i]
		if b.Name != w.name || b.Kind != w.kind || b.SkipOnRollback != w.skip || b.Timeout != w.timeout || b.Phase != w.phase || b.Gate == nil {
			t.Errorf("binding %d = %+v, want %+v", i, b, w)
		}
	}
}

func TestBuiltinsOnRollback(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.clock.now = time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	f.apps.set(healthyApp, nil)
	f.freezes.locks = []core.Lock{freeze("alice", "incident", false)}
	catalog := loadCatalog(t, allGatesConfig)
	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, f.deps()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	plans, err := r.Plans(catalog)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	h := newHarness(t, plans["web"], func(o *gates.Options) { o.Now = f.clock.Now })
	d := deployment(core.KindRollback)

	before := mustEvaluate(t, h, d, core.GatePhaseBefore)
	if got, want := strings.Join(outcomeStatuses(before), " "), "checks=skipped lock=skipped schedule=skipped"; got != want || before.Status != core.GatePassed {
		t.Errorf("before gates for a rollback = %s (%s), want %s and passed", got, before.Status, want)
	}
	after := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if got, want := strings.Join(outcomeStatuses(after), " "), "health=passed soak=skipped"; got != want || after.Status != core.GatePassed {
		t.Errorf("after gates for a rollback = %s (%s), want %s and passed", got, after.Status, want)
	}
	for _, res := range append(before.Results, after.Results...) {
		if res.Status == core.GateSkipped && res.Message != "skipped for rollback" {
			t.Errorf("%s message = %q, want skipped for rollback", res.Name, res.Message)
		}
	}
	if len(f.tracked.calls) != 0 {
		t.Errorf("checks gate read tracked commits for a rollback: %v", f.tracked.calls)
	}
}

func TestBuiltinsOnDeploy(t *testing.T) {
	t.Parallel()

	f := newBuiltinFakes()
	f.clock.now = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	f.apps.set(healthyApp, nil)
	f.tracked.commits = []core.TrackedCommit{trackedCommit(core.Target{App: "web", Environment: "prod"}, core.ChecksSuccess)}
	catalog := loadCatalog(t, allGatesConfig)
	r := gates.NewRegistry()
	if err := gates.RegisterBuiltins(r, f.deps()); err != nil {
		t.Fatalf("RegisterBuiltins: %v", err)
	}
	plans, err := r.Plans(catalog)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	h := newHarness(t, plans["web"], func(o *gates.Options) { o.Now = f.clock.Now })
	d := deployment(core.KindDeploy)

	before := mustEvaluate(t, h, d, core.GatePhaseBefore)
	if got, want := strings.Join(outcomeStatuses(before), " "), "checks=passed lock=passed schedule=passed"; got != want {
		t.Errorf("before gates = %s, want %s", got, want)
	}
	after := mustEvaluate(t, h, d, core.GatePhaseAfter)
	if got, want := strings.Join(outcomeStatuses(after), " "), "health=passed soak=pending"; got != want {
		t.Errorf("after gates = %s, want %s", got, want)
	}
	f.clock.Advance(30 * time.Minute)
	h.clock.Advance(30 * time.Minute)
	after = mustEvaluate(t, h, d, core.GatePhaseAfter)
	if got, want := strings.Join(outcomeStatuses(after), " "), "health=passed soak=passed"; got != want || after.Status != core.GatePassed {
		t.Errorf("after the soak = %s (%s), want %s", got, after.Status, want)
	}
}
