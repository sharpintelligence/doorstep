package gates

import (
	"context"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type Observer interface {
	GateRecorded(ctx context.Context, d core.Deployment, result core.GateResult) error
}
