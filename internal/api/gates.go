package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

const (
	deploymentGatePath = Prefix + "deployments/{id}/gates/{gate}"
	commitGatePath     = Prefix + "apps/{app}/commits/{sha}/gates/{gate}"

	gateNotReadyRetryAfter = "5"
)

type gateReportRequest struct {
	Status  core.GateStatus `json:"status"`
	Message string          `json:"message"`
	URL     string          `json:"url"`
	Details json.RawMessage `json:"details"`
}

type gateReportResponse struct {
	DeploymentID core.DeploymentID `json:"deployment_id"`
	App          string            `json:"app"`
	Gate         string            `json:"gate"`
	Phase        core.GatePhase    `json:"phase"`
	Status       core.GateStatus   `json:"status"`
	Message      string            `json:"message"`
	URL          string            `json:"url"`
	Details      json.RawMessage   `json:"details"`
	ReportedBy   string            `json:"reported_by"`
	ReportedAt   time.Time         `json:"reported_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func (a *api) registerGateReports(mux *http.ServeMux) {
	mux.Handle("POST "+deploymentGatePath, a.authenticated(core.RoleReporter, a.reportDeploymentGate))
	mux.HandleFunc(deploymentGatePath, methodNotAllowed(http.MethodPost))
	mux.Handle("POST "+commitGatePath, a.authenticated(core.RoleReporter, a.reportCommitGate))
	mux.HandleFunc(commitGatePath, methodNotAllowed(http.MethodPost))
}

func (a *api) reportDeploymentGate(w http.ResponseWriter, r *http.Request, caller Caller) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "deployment id must be a positive integer")
		return
	}
	a.reportGate(w, r, caller, gates.ReportRequest{DeploymentID: id})
}

func (a *api) reportCommitGate(w http.ResponseWriter, r *http.Request, caller Caller) {
	app, sha := r.PathValue("app"), r.PathValue("sha")
	if !a.known(app) {
		writeError(w, http.StatusNotFound, codeNotFound, "unknown app "+app)
		return
	}
	if err := core.ValidateCommitSHA(sha); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "commit SHA must be 40 lowercase hex characters")
		return
	}
	a.reportGate(w, r, caller, gates.ReportRequest{App: app, CommitSHA: sha})
}

func (a *api) reportGate(w http.ResponseWriter, r *http.Request, caller Caller, req gates.ReportRequest) {
	var body gateReportRequest
	if reqErr := decodeJSON(w, r, &body); reqErr != nil {
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
		return
	}
	if body.Status == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "status is required (want pending, passed or failed)")
		return
	}
	if bytes.Equal(bytes.TrimSpace(body.Details), []byte("null")) {
		body.Details = nil
	}
	req.Gate = r.PathValue("gate")
	req.Status = body.Status
	req.Message = body.Message
	req.URL = body.URL
	req.Details = body.Details
	req.Actor = caller.Actor
	req.Role = caller.Role

	result, err := a.reports.Report(r.Context(), req)
	if err != nil {
		a.gateReportError(w, r, req, err)
		return
	}
	writeJSON(w, http.StatusOK, newGateReportResponse(result))
}

func (a *api) gateReportError(w http.ResponseWriter, r *http.Request, req gates.ReportRequest, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, notFoundMessage(req))
	case errors.Is(err, gates.ErrUnknownGate):
		writeError(w, http.StatusNotFound, codeNotFound, "unknown gate "+req.Gate)
	case errors.Is(err, core.ErrInvalidGateReport):
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
	case errors.Is(err, gates.ErrGateNotExternal):
		writeError(w, http.StatusConflict, codeGateNotExternal, err.Error())
	case errors.Is(err, gates.ErrDeploymentFinished):
		writeError(w, http.StatusConflict, codeDeploymentFinished, err.Error())
	case errors.Is(err, gates.ErrGateNotApplicable):
		writeError(w, http.StatusConflict, codeGateNotApplicable, err.Error())
	case errors.Is(err, gates.ErrGateNotReady):
		w.Header().Set("Retry-After", gateNotReadyRetryAfter)
		writeError(w, http.StatusConflict, codeGateNotReady, err.Error())
	case errors.Is(err, core.ErrGateDecided):
		writeError(w, http.StatusConflict, codeGateDecided, "gate "+req.Gate+" already has a final result; only the same verdict can be reported again")
	default:
		a.internalError(w, r, err)
	}
}

func notFoundMessage(req gates.ReportRequest) string {
	if req.DeploymentID != 0 {
		return fmt.Sprintf("deployment %d not found", req.DeploymentID)
	}
	return fmt.Sprintf("no unfinished deployment of %s for commit %s", req.App, req.CommitSHA)
}

func newGateReportResponse(res gates.ReportResult) gateReportResponse {
	return gateReportResponse{
		DeploymentID: res.Deployment.ID,
		App:          res.Deployment.Target.App,
		Gate:         res.Report.Gate,
		Phase:        res.Phase,
		Status:       res.Report.Status,
		Message:      res.Report.Message,
		URL:          res.Report.URL,
		Details:      jsonObject(res.Report.Details),
		ReportedBy:   res.Report.ReportedBy,
		ReportedAt:   res.Report.CreatedAt.UTC(),
		UpdatedAt:    res.Report.UpdatedAt.UTC(),
	}
}
