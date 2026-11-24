package gates

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

type Gate interface {
	Evaluate(ctx context.Context, req Request) (Evaluation, error)
}

type Request struct {
	Deployment core.Deployment
	Phase      core.GatePhase
	Previous   *core.GateResult
}

type Evaluation struct {
	Status   core.GateStatus
	Message  string
	Evidence json.RawMessage
}

type GateFunc func(ctx context.Context, req Request) (Evaluation, error)

func (f GateFunc) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	return f(ctx, req)
}

type Binding struct {
	Name           string
	Type           config.GateType
	Kind           metrics.GateKind
	Phase          core.GatePhase
	Timeout        time.Duration
	SkipOnRollback bool
	Gate           Gate
}

type Plan struct {
	Before []Binding
	After  []Binding
}

func (p Plan) Phase(phase core.GatePhase) []Binding {
	switch phase {
	case core.GatePhaseBefore:
		return p.Before
	case core.GatePhaseAfter:
		return p.After
	default:
		return nil
	}
}
