package gates

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Observer interface {
	GateRecorded(ctx context.Context, d core.Deployment, result core.GateResult) error
}

type ObserverFunc func(ctx context.Context, d core.Deployment, result core.GateResult) error

func (f ObserverFunc) GateRecorded(ctx context.Context, d core.Deployment, result core.GateResult) error {
	return f(ctx, d, result)
}
