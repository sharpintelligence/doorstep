package gates

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

var (
	ErrUnknownGateType    = errors.New("unknown gate type")
	ErrDuplicateGateType  = errors.New("gate type already registered")
	ErrInvalidDefinition  = errors.New("invalid gate definition")
	errFactoryReturnedNil = errors.New("factory returned no gate")
)

type Factory func(app config.App, gate config.Gate) (Gate, error)

type Definition struct {
	Kind           metrics.GateKind
	SkipOnRollback bool
	New            Factory
}

type Registry struct {
	definitions map[config.GateType]Definition
}

func NewRegistry() *Registry {
	return &Registry{definitions: map[config.GateType]Definition{}}
}

func (r *Registry) Register(t config.GateType, d Definition) error {
	switch {
	case strings.TrimSpace(string(t)) == "":
		return fmt.Errorf("%w: empty gate type", ErrInvalidDefinition)
	case d.New == nil:
		return fmt.Errorf("%w: gate type %q has no factory", ErrInvalidDefinition, t)
	case strings.TrimSpace(string(d.Kind)) == "":
		return fmt.Errorf("%w: gate type %q has no metrics kind", ErrInvalidDefinition, t)
	}
	if _, ok := r.definitions[t]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateGateType, t)
	}
	r.definitions[t] = d
	return nil
}

func (r *Registry) Plan(app config.App) (Plan, error) {
	before, beforeErr := r.bindings(app, core.GatePhaseBefore, app.Gates.Before)
	after, afterErr := r.bindings(app, core.GatePhaseAfter, app.Gates.After)
	if err := errors.Join(beforeErr, afterErr); err != nil {
		return Plan{}, err
	}
	return Plan{Before: before, After: after}, nil
}

func (r *Registry) Plans(c config.Catalog) (map[string]Plan, error) {
	plans := make(map[string]Plan, len(c.Apps))
	var errs []error
	for _, app := range c.Apps {
		plan, err := r.Plan(app)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		plans[app.Name] = plan
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return plans, nil
}

func (r *Registry) bindings(app config.App, phase core.GatePhase, gates []config.Gate) ([]Binding, error) {
	bindings := make([]Binding, 0, len(gates))
	var errs []error
	for _, g := range gates {
		binding, err := r.binding(app, phase, g)
		if err != nil {
			errs = append(errs, fmt.Errorf("app %s: gates.%s %s: %w", app.Name, phase, g.Name, err))
			continue
		}
		bindings = append(bindings, binding)
	}
	return bindings, errors.Join(errs...)
}

func (r *Registry) binding(app config.App, phase core.GatePhase, g config.Gate) (Binding, error) {
	if strings.TrimSpace(g.Name) == "" {
		return Binding{}, errors.New("gate has no name")
	}
	def, ok := r.definitions[g.Type]
	if !ok {
		return Binding{}, fmt.Errorf("%w %q", ErrUnknownGateType, g.Type)
	}
	gate, err := def.New(app, g)
	if err != nil {
		return Binding{}, fmt.Errorf("build %s gate: %w", g.Type, err)
	}
	if gate == nil {
		return Binding{}, fmt.Errorf("build %s gate: %w", g.Type, errFactoryReturnedNil)
	}
	b := Binding{
		Name:           g.Name,
		Type:           g.Type,
		Kind:           def.Kind,
		Phase:          phase,
		SkipOnRollback: def.SkipOnRollback,
		Gate:           gate,
	}
	if g.Timeout != nil {
		b.Timeout = g.Timeout.Std()
	}
	return b, nil
}
