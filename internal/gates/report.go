package gates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

var (
	ErrUnknownGate        = errors.New("unknown gate")
	ErrGateNotExternal    = errors.New("gate does not accept external results")
	ErrDeploymentFinished = errors.New("deployment has finished")
	ErrGateNotApplicable  = errors.New("gate does not apply to rollbacks")
	ErrGateNotReady       = errors.New("gate is not being evaluated yet")
	ErrReporterNotAllowed = errors.New("caller may not report this gate")
)

type ReportStore interface {
	GetDeployment(ctx context.Context, id core.DeploymentID) (core.Deployment, error)
	ActiveDeploymentForCommit(ctx context.Context, t core.Target, sha string) (core.Deployment, error)
	SaveGateReport(ctx context.Context, r core.GateReport, audit core.AuditEntry) (core.GateReport, bool, error)
}

type ReportRequest struct {
	DeploymentID core.DeploymentID
	App          string
	CommitSHA    string
	Gate         string
	Status       core.GateStatus
	Message      string
	URL          string
	Details      json.RawMessage
	Actor        core.Actor
	Role         core.Role
}

type ReportResult struct {
	Report     core.GateReport
	Deployment core.Deployment
	Phase      core.GatePhase
	Changed    bool
}

type Reporter struct {
	store   ReportStore
	catalog config.Catalog
}

type reportAuditDetail struct {
	Gate    string          `json:"gate"`
	Phase   core.GatePhase  `json:"phase"`
	Status  core.GateStatus `json:"status"`
	Message string          `json:"message,omitempty"`
	URL     string          `json:"url,omitempty"`
}

func NewReporter(store ReportStore, catalog config.Catalog) (*Reporter, error) {
	if store == nil {
		return nil, errors.New("gate reporter: a store is required")
	}
	return &Reporter{store: store, catalog: catalog}, nil
}

func (r *Reporter) Report(ctx context.Context, req ReportRequest) (ReportResult, error) {
	d, err := r.deployment(ctx, req)
	if err != nil {
		return ReportResult{}, err
	}
	gate, phase, err := r.externalGate(d, req.Gate)
	if err != nil {
		return ReportResult{}, err
	}
	if gate.Reporters != nil && !slices.Contains(gate.Reporters, req.Actor.String()) {
		return ReportResult{}, fmt.Errorf("%w: %s is not in the reporters of gate %s of app %s", ErrReporterNotAllowed, req.Actor, gate.Name, d.Target.App)
	}
	switch {
	case d.State.Terminal():
		return ReportResult{}, fmt.Errorf("%w: deployment %d is %s", ErrDeploymentFinished, d.ID, d.State)
	case d.Kind == core.KindRollback && !gate.ApplyToRollbacks:
		return ReportResult{}, fmt.Errorf("%w: gate %s is skipped for rollback deployment %d", ErrGateNotApplicable, gate.Name, d.ID)
	case phase == core.GatePhaseAfter && d.State != core.StateVerifying:
		return ReportResult{}, fmt.Errorf("%w: gate %s runs after deploy and deployment %d is %s", ErrGateNotReady, gate.Name, d.ID, d.State)
	}

	report := core.GateReport{
		DeploymentID: d.ID,
		Gate:         gate.Name,
		Status:       req.Status,
		Message:      core.CleanGateMessage(req.Message),
		URL:          req.URL,
		Details:      req.Details,
		ReportedBy:   req.Actor.String(),
	}
	if err := report.Validate(); err != nil {
		return ReportResult{}, err
	}
	detail, err := json.Marshal(reportAuditDetail{Gate: gate.Name, Phase: phase, Status: report.Status, Message: report.Message, URL: report.URL})
	if err != nil {
		return ReportResult{}, fmt.Errorf("encode gate report audit detail: %w", err)
	}
	audit := core.AuditEntry{
		Actor:        req.Actor,
		Role:         req.Role,
		Action:       core.AuditGateReport,
		Target:       &d.Target,
		DeploymentID: &d.ID,
		Detail:       detail,
	}
	stored, changed, err := r.store.SaveGateReport(ctx, report, audit)
	if err != nil {
		return ReportResult{}, err
	}
	return ReportResult{Report: stored, Deployment: d, Phase: phase, Changed: changed}, nil
}

func (r *Reporter) deployment(ctx context.Context, req ReportRequest) (core.Deployment, error) {
	if req.DeploymentID != 0 {
		return r.store.GetDeployment(ctx, req.DeploymentID)
	}
	if _, ok := r.catalog.App(req.App); !ok {
		return core.Deployment{}, fmt.Errorf("app %s: %w", req.App, core.ErrNotFound)
	}
	return r.store.ActiveDeploymentForCommit(ctx, core.AppTarget(req.App), req.CommitSHA)
}

func (r *Reporter) externalGate(d core.Deployment, name string) (config.Gate, core.GatePhase, error) {
	app, ok := r.catalog.App(d.Target.App)
	if !ok {
		return config.Gate{}, "", fmt.Errorf("%w %s: app %s is not configured", ErrUnknownGate, name, d.Target.App)
	}
	for _, phase := range []struct {
		phase core.GatePhase
		gates []config.Gate
	}{{core.GatePhaseBefore, app.Gates.Before}, {core.GatePhaseAfter, app.Gates.After}} {
		for _, g := range phase.gates {
			if g.Name != name {
				continue
			}
			if g.Type != config.GateExternal {
				return config.Gate{}, "", fmt.Errorf("%w: gate %s of app %s has type %s", ErrGateNotExternal, name, app.Name, g.Type)
			}
			return g, phase.phase, nil
		}
	}
	return config.Gate{}, "", fmt.Errorf("%w %s for app %s", ErrUnknownGate, name, app.Name)
}
