package gates

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

const DefaultDegradedGrace = time.Minute

type Builtins struct {
	Tracked       TrackedChecks
	Live          LiveChecks
	Freezes       FreezeReader
	Applications  ApplicationReader
	Now           func() time.Time
	DegradedGrace time.Duration
}

func RegisterBuiltins(r *Registry, deps Builtins) error {
	if r == nil {
		return errors.New("register built-in gates: no registry")
	}
	var errs []error
	if deps.Tracked == nil {
		errs = append(errs, errors.New("the checks gate needs tracked commits"))
	}
	if deps.Freezes == nil {
		errs = append(errs, errors.New("the lock gate needs a freeze reader"))
	}
	if deps.Applications == nil {
		errs = append(errs, errors.New("the health and soak gates need an Argo CD application reader"))
	}
	if deps.DegradedGrace < 0 {
		errs = append(errs, fmt.Errorf("degraded grace %s is negative", deps.DegradedGrace))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("register built-in gates: %w", err)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.DegradedGrace == 0 {
		deps.DegradedGrace = DefaultDegradedGrace
	}

	definitions := []struct {
		typ config.GateType
		def Definition
	}{
		{config.GateChecks, Definition{Kind: metrics.GateKindCI, SkipOnRollback: true, New: deps.newChecks}},
		{config.GateLock, Definition{Kind: metrics.GateKindLock, SkipOnRollback: true, New: deps.newLock}},
		{config.GateSchedule, Definition{Kind: metrics.GateKindSchedule, New: deps.newSchedule}},
		{config.GateHealth, Definition{Kind: metrics.GateKindArgoCD, New: deps.newHealth}},
		{config.GateSoak, Definition{Kind: metrics.GateKindSoak, SkipOnRollback: true, New: deps.newSoak}},
	}
	for _, d := range definitions {
		if err := r.Register(d.typ, d.def); err != nil {
			return fmt.Errorf("register built-in gates: %w", err)
		}
	}
	return nil
}

func clean(s string) string {
	return core.CleanGateMessage(s)
}

func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func parseTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func marshalEvidence(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode gate evidence: %w", err)
	}
	return data, nil
}
