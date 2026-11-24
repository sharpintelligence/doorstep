package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

const waitingForExternal = "waiting for an external result"

type externalGate struct {
	name             string
	applyToRollbacks bool
	reports          ExternalReports
}

type externalEvidence struct {
	Reported   bool            `json:"reported"`
	Status     core.GateStatus `json:"status,omitempty"`
	ReportedBy string          `json:"reported_by,omitempty"`
	ReportedAt string          `json:"reported_at,omitempty"`
	URL        string          `json:"url,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
}

func RegisterExternal(r *Registry, reports ExternalReports) error {
	switch {
	case r == nil:
		return errors.New("register external gate: no registry")
	case reports == nil:
		return errors.New("register external gate: the external gate needs a report reader")
	}
	err := r.Register(config.GateExternal, Definition{
		Kind: metrics.GateKindExternal,
		New: func(_ config.App, g config.Gate) (Gate, error) {
			return externalGate{name: g.Name, applyToRollbacks: g.ApplyToRollbacks, reports: reports}, nil
		},
	})
	if err != nil {
		return fmt.Errorf("register external gate: %w", err)
	}
	return nil
}

func (g externalGate) Evaluate(ctx context.Context, req Request) (Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return Evaluation{}, err
	}
	if req.Deployment.Kind == core.KindRollback && !g.applyToRollbacks {
		return Evaluation{Status: core.GateSkipped, Message: skippedForRollback}, nil
	}
	report, err := g.reports.GetGateReport(ctx, req.Deployment.ID, g.name)
	if errors.Is(err, core.ErrNotFound) {
		return externalEvaluation(core.GatePending, waitingForExternal, externalEvidence{})
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("read external result of gate %s: %w", g.name, err)
	}
	ev := externalEvidence{
		Reported:   true,
		Status:     report.Status,
		ReportedBy: report.ReportedBy,
		ReportedAt: timestamp(report.UpdatedAt),
		URL:        report.URL,
	}
	if len(report.Details) > 0 && string(report.Details) != "{}" {
		ev.Details = report.Details
	}
	message := report.Message
	if message == "" {
		message = "reported " + string(report.Status) + " by " + report.ReportedBy
	}
	return externalEvaluation(report.Status, message, ev)
}

func externalEvaluation(status core.GateStatus, message string, ev externalEvidence) (Evaluation, error) {
	evidence, err := marshalEvidence(ev)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Status: status, Message: message, Evidence: evidence}, nil
}
