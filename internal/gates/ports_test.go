package gates_test

import (
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/intake"
	"github.com/sharpintelligence/doorstep/internal/locks"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/notify"
	"github.com/sharpintelligence/doorstep/internal/store"
)

var (
	_ gates.ResultStore     = (*store.Store)(nil)
	_ gates.Metrics         = (*metrics.Recorder)(nil)
	_ gates.TrackedChecks   = (*store.Store)(nil)
	_ gates.FreezeReader    = (*locks.Service)(nil)
	_ gates.LiveChecks      = intake.LiveChecks{}
	_ gates.ExternalReports = (*store.Store)(nil)
	_ gates.ReportStore     = (*store.Store)(nil)
	_ gates.Observer        = (*notify.GateStatuses)(nil)
)
