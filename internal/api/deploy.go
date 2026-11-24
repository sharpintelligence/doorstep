package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
)

const (
	cancelPath            = Prefix + "deployments/{id}/cancel"
	unavailableRetryAfter = "5"
)

type deployRequest struct {
	CommitSHA string `json:"commit_sha"`
	Urgent    bool   `json:"urgent"`
	Reason    string `json:"reason"`
}

type deployResponse struct {
	Deployment deployment `json:"deployment"`
	Created    bool       `json:"created"`
}

type cancelRequest struct {
	Reason string `json:"reason"`
}

type cancelResponse struct {
	Deployment deployment `json:"deployment"`
}

func (a *api) deploysEnabled() bool {
	return a.deploys != nil && a.known != nil && a.auth != nil
}

func (a *api) registerDeploys(mux *http.ServeMux) {
	mux.Handle("POST "+appDeploymentsPath, a.authenticated(core.RoleDeployer, a.deployApp))
	mux.Handle("POST "+cancelPath, a.authenticated(core.RoleDeployer, a.cancelDeployment))
	mux.HandleFunc(cancelPath, methodNotAllowed(http.MethodPost))
}

func (a *api) deployApp(w http.ResponseWriter, r *http.Request, caller Caller) {
	app := r.PathValue("app")
	if !a.known(app) {
		writeError(w, http.StatusNotFound, codeNotFound, "unknown app "+app)
		return
	}
	var body deployRequest
	if reqErr := decodeJSON(w, r, &body); reqErr != nil {
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
		return
	}
	if body.CommitSHA == "" {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "commit_sha is required")
		return
	}
	result, err := a.deploys.Deploy(r.Context(), deploys.Request{
		Target:    core.AppTarget(app),
		CommitSHA: body.CommitSHA,
		Urgent:    body.Urgent,
		Reason:    body.Reason,
		Actor:     caller.Actor,
		Role:      caller.Role,
	})
	if err != nil {
		a.deployError(w, r, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", Prefix+"deployments/"+strconv.FormatInt(int64(result.Deployment.ID), 10))
	writeJSON(w, status, deployResponse{Deployment: newDeployment(result.Deployment), Created: result.Created})
}

func (a *api) cancelDeployment(w http.ResponseWriter, r *http.Request, caller Caller) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "deployment id must be a positive integer")
		return
	}
	var body cancelRequest
	if reqErr := decodeOptionalJSON(w, r, &body); reqErr != nil {
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
		return
	}
	d, err := a.deploys.Cancel(r.Context(), deploys.CancelRequest{ID: id, Reason: body.Reason, Actor: caller.Actor, Role: caller.Role})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, cancelResponse{Deployment: newDeployment(d)})
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, fmt.Sprintf("deployment %d not found", id))
	default:
		a.deployError(w, r, err)
	}
}

func (a *api) deployError(w http.ResponseWriter, r *http.Request, err error) {
	if blocked, ok := errors.AsType[*core.BlockedError](err); ok {
		writeJSON(w, http.StatusConflict, blockedResponse{
			Error: errorBody{Code: codeBlocked, Message: blocked.Error()},
			Block: newBlockResponse(blocked.Block),
		})
		return
	}
	switch {
	case errors.Is(err, deploys.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
	case errors.Is(err, deploys.ErrUnknownApp), errors.Is(err, deploys.ErrCommitNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error())
	case errors.Is(err, deploys.ErrCommitNotOnBranch):
		writeError(w, http.StatusConflict, codeCommitNotOnBranch, err.Error())
	case errors.Is(err, deploys.ErrNotCancellable):
		writeError(w, http.StatusConflict, codeNotCancellable, err.Error())
	case errors.Is(err, deploys.ErrUnavailable):
		a.logger.WarnContext(r.Context(), "deploy request could not be checked", slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		w.Header().Set("Retry-After", unavailableRetryAfter)
		writeError(w, http.StatusServiceUnavailable, codeUnavailable, "GitHub or migration detection is unavailable; retry later")
	case errors.Is(err, core.ErrStateConflict):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, codeContended, "the deployment changed while it was being cancelled; retry")
	default:
		a.internalError(w, r, err)
	}
}
