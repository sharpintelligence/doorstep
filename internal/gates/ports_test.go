package gates_test

import (
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/store"
)

var (
	_ gates.ResultStore = (*store.Store)(nil)
	_ gates.Metrics     = (*metrics.Recorder)(nil)
	_ gates.Gate        = gates.GateFunc(nil)
	_ gates.Observer    = gates.ObserverFunc(nil)
)
