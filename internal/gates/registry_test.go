package gates_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

type builtGate struct {
	app  string
	gate config.Gate
}

func (builtGate) Evaluate(context.Context, gates.Request) (gates.Evaluation, error) {
	return gates.Evaluation{Status: core.GatePassed}, nil
}

func buildGate(app config.App, g config.Gate) (gates.Gate, error) {
	return builtGate{app: app.Name, gate: g}, nil
}

func standardRegistry(t *testing.T) *gates.Registry {
	t.Helper()
	r := gates.NewRegistry()
	defs := map[config.GateType]gates.Definition{
		config.GateChecks: {Kind: metrics.GateKindCI, SkipOnRollback: true, New: buildGate},
		config.GateHealth: {Kind: metrics.GateKindArgoCD, New: buildGate},
		config.GateSoak:   {Kind: metrics.GateKindSoak, SkipOnRollback: true, New: buildGate},
	}
	for typ, def := range defs {
		if err := r.Register(typ, def); err != nil {
			t.Fatalf("Register(%s): %v", typ, err)
		}
	}
	return r
}

func loadCatalog(t *testing.T, yaml string) config.Catalog {
	t.Helper()
	f, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return f.Catalog()
}

func TestRegistryRegister(t *testing.T) {
	t.Parallel()

	valid := gates.Definition{Kind: metrics.GateKindCI, New: buildGate}
	tests := []struct {
		name string
		typ  config.GateType
		def  gates.Definition
		want error
	}{
		{name: "valid", typ: config.GateChecks, def: valid},
		{name: "empty type", typ: "", def: valid, want: gates.ErrInvalidDefinition},
		{name: "blank type", typ: " ", def: valid, want: gates.ErrInvalidDefinition},
		{name: "nil factory", typ: config.GateHealth, def: gates.Definition{Kind: metrics.GateKindArgoCD}, want: gates.ErrInvalidDefinition},
		{name: "no kind", typ: config.GateHealth, def: gates.Definition{New: buildGate}, want: gates.ErrInvalidDefinition},
		{name: "empty definition", typ: config.GateHealth, want: gates.ErrInvalidDefinition},
		{name: "duplicate", typ: config.GateSoak, def: valid, want: gates.ErrDuplicateGateType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gates.NewRegistry()
			if err := r.Register(config.GateSoak, gates.Definition{Kind: metrics.GateKindSoak, New: buildGate}); err != nil {
				t.Fatalf("Register(soak): %v", err)
			}
			err := r.Register(tt.typ, tt.def)
			if tt.want == nil && err != nil {
				t.Fatalf("Register: %v", err)
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("Register = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRegistryPlanFromCatalog(t *testing.T) {
	t.Parallel()

	c := loadCatalog(t, `
apps:
  - name: web
  - name: api
    gates:
      before: []
      after:
        - type: health
          timeout: 3m
        - type: soak
          name: bake
          duration: 15m
`)
	plans, err := standardRegistry(t).Plans(c)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans for %d apps, want 2", len(plans))
	}

	type want struct {
		name           string
		typ            config.GateType
		kind           metrics.GateKind
		phase          core.GatePhase
		timeout        time.Duration
		skipOnRollback bool
	}
	tests := []struct {
		app    string
		before []want
		after  []want
	}{
		{
			app:    "web",
			before: []want{{name: "checks", typ: config.GateChecks, kind: metrics.GateKindCI, phase: core.GatePhaseBefore, timeout: 30 * time.Minute, skipOnRollback: true}},
			after:  []want{{name: "health", typ: config.GateHealth, kind: metrics.GateKindArgoCD, phase: core.GatePhaseAfter, timeout: 10 * time.Minute}},
		},
		{
			app: "api",
			after: []want{
				{name: "health", typ: config.GateHealth, kind: metrics.GateKindArgoCD, phase: core.GatePhaseAfter, timeout: 3 * time.Minute},
				{name: "bake", typ: config.GateSoak, kind: metrics.GateKindSoak, phase: core.GatePhaseAfter, skipOnRollback: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.app, func(t *testing.T) {
			t.Parallel()
			plan := plans[tt.app]
			for phase, wants := range map[core.GatePhase][]want{core.GatePhaseBefore: tt.before, core.GatePhaseAfter: tt.after} {
				got := plan.Phase(phase)
				if len(got) != len(wants) {
					t.Fatalf("%s bindings = %d, want %d", phase, len(got), len(wants))
				}
				for i, w := range wants {
					b := got[i]
					if b.Name != w.name || b.Type != w.typ || b.Kind != w.kind || b.Phase != w.phase || b.Timeout != w.timeout || b.SkipOnRollback != w.skipOnRollback {
						t.Errorf("%s[%d] = %+v, want %+v", phase, i, b, w)
					}
					built, ok := b.Gate.(builtGate)
					if !ok || built.app != tt.app || built.gate.Name != w.name {
						t.Errorf("%s[%d] gate = %#v, want one built for %s/%s", phase, i, b.Gate, tt.app, w.name)
					}
				}
			}
		})
	}
}

func TestPlanPhase(t *testing.T) {
	t.Parallel()

	plan := gates.Plan{
		Before: []gates.Binding{{Name: "checks"}},
		After:  []gates.Binding{{Name: "health"}, {Name: "soak"}},
	}
	tests := []struct {
		phase core.GatePhase
		want  int
	}{
		{core.GatePhaseBefore, 1},
		{core.GatePhaseAfter, 2},
		{"during", 0},
	}
	for _, tt := range tests {
		if got := len(plan.Phase(tt.phase)); got != tt.want {
			t.Errorf("Phase(%q) has %d bindings, want %d", tt.phase, got, tt.want)
		}
	}
}

func TestRegistryPlanErrors(t *testing.T) {
	t.Parallel()

	errFactory := errors.New("bad settings")
	tests := []struct {
		name     string
		register map[config.GateType]gates.Definition
		app      config.App
		want     error
		contains []string
	}{
		{
			name: "unregistered type",
			app:  config.App{Name: "web", Gates: config.Gates{After: []config.Gate{{Type: config.GateHealth, Name: "health"}}}},
			want: gates.ErrUnknownGateType,
			contains: []string{
				`app web: gates.after health: unknown gate type "health"`,
			},
		},
		{
			name: "factory error",
			register: map[config.GateType]gates.Definition{config.GateChecks: {Kind: metrics.GateKindCI, New: func(config.App, config.Gate) (gates.Gate, error) {
				return nil, errFactory
			}}},
			app:      config.App{Name: "web", Gates: config.Gates{Before: []config.Gate{{Type: config.GateChecks, Name: "ci"}}}},
			want:     errFactory,
			contains: []string{"app web: gates.before ci: build checks gate: bad settings"},
		},
		{
			name: "factory returns nil",
			register: map[config.GateType]gates.Definition{config.GateChecks: {Kind: metrics.GateKindCI, New: func(config.App, config.Gate) (gates.Gate, error) {
				return nil, nil
			}}},
			app:      config.App{Name: "web", Gates: config.Gates{Before: []config.Gate{{Type: config.GateChecks, Name: "ci"}}}},
			contains: []string{"build checks gate: factory returned no gate"},
		},
		{
			name:     "unnamed gate",
			register: map[config.GateType]gates.Definition{config.GateChecks: {Kind: metrics.GateKindCI, New: buildGate}},
			app:      config.App{Name: "web", Gates: config.Gates{Before: []config.Gate{{Type: config.GateChecks}}}},
			contains: []string{"gate has no name"},
		},
		{
			name: "errors from both phases",
			app: config.App{Name: "web", Gates: config.Gates{
				Before: []config.Gate{{Type: config.GateChecks, Name: "ci"}},
				After:  []config.Gate{{Type: config.GateSoak, Name: "bake"}},
			}},
			want:     gates.ErrUnknownGateType,
			contains: []string{"gates.before ci", "gates.after bake"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := gates.NewRegistry()
			for typ, def := range tt.register {
				if err := r.Register(typ, def); err != nil {
					t.Fatalf("Register(%s): %v", typ, err)
				}
			}
			plan, err := r.Plan(tt.app)
			if err == nil {
				t.Fatalf("Plan = %+v, want an error", plan)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			for _, s := range tt.contains {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("err %q does not contain %q", err, s)
				}
			}
		})
	}
}

func TestRegistryPlansJoinsErrors(t *testing.T) {
	t.Parallel()

	c := loadCatalog(t, `
apps:
  - name: web
  - name: api
    gates:
      before: []
      after: []
  - name: worker
`)
	plans, err := gates.NewRegistry().Plans(c)
	if plans != nil {
		t.Errorf("plans = %+v, want nil on error", plans)
	}
	if !errors.Is(err, gates.ErrUnknownGateType) {
		t.Fatalf("err = %v, want ErrUnknownGateType", err)
	}
	for _, s := range []string{"app web: gates.before checks", "app web: gates.after health", "app worker: gates.before checks", "app worker: gates.after health"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("err %q does not contain %q", err, s)
		}
	}
	if strings.Contains(err.Error(), "app api") {
		t.Errorf("err %q mentions an app with no gates", err)
	}

	empty, err := gates.NewRegistry().Plans(loadCatalog(t, "apps:\n  - name: api\n    gates:\n      before: []\n      after: []\n"))
	if err != nil {
		t.Fatalf("Plans without gates: %v", err)
	}
	if p, ok := empty["api"]; !ok || len(p.Before) != 0 || len(p.After) != 0 {
		t.Errorf("plans = %+v, want an empty plan for api", empty)
	}
}
