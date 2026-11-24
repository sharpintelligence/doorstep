package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
)

const appRollbackPath = Prefix + "apps/{app}/rollback"

type rollbackRequest struct {
	ToDeploymentID           *core.DeploymentID `json:"to_deployment_id"`
	ToDigest                 *string            `json:"to_digest"`
	Reason                   string             `json:"reason"`
	ConfirmMigrationRollback bool               `json:"confirm_migration_rollback"`
}

type rollbackResponse struct {
	Deployment deployment          `json:"deployment"`
	RolledBack deployment          `json:"rolled_back"`
	Block      *blockResponse      `json:"block"`
	Cancelled  []core.DeploymentID `json:"cancelled"`
	Migrations []deployment        `json:"migrations"`
}

type migrationConfirmationResponse struct {
	Error      errorBody    `json:"error"`
	To         deployment   `json:"to"`
	Migrations []deployment `json:"migrations"`
}

type rollbackInProgressResponse struct {
	Error      errorBody  `json:"error"`
	Deployment deployment `json:"deployment"`
}

type blockedResponse struct {
	Error errorBody     `json:"error"`
	Block blockResponse `json:"block"`
}

func (a *api) registerRollback(mux *http.ServeMux) {
	mux.Handle("POST "+appRollbackPath, a.authenticated(core.RoleDeployer, a.rollbackApp))
	mux.HandleFunc(appRollbackPath, methodNotAllowed(http.MethodPost))
}

func (a *api) rollbackApp(w http.ResponseWriter, r *http.Request, caller Caller) {
	app := r.PathValue("app")
	if !a.known(app) {
		writeError(w, http.StatusNotFound, codeNotFound, "unknown app "+app)
		return
	}
	var body rollbackRequest
	if reqErr := decodeOptionalJSON(w, r, &body); reqErr != nil {
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
		return
	}
	req := rollback.Request{
		Target:           core.AppTarget(app),
		ToDeployment:     body.ToDeploymentID,
		Reason:           body.Reason,
		ConfirmMigration: body.ConfirmMigrationRollback,
		Actor:            caller.Actor,
		Role:             caller.Role,
	}
	if body.ToDigest != nil {
		if body.ToDeploymentID != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "set at most one of to_deployment_id and to_digest")
			return
		}
		digest, err := core.ParseDigest(*body.ToDigest)
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "to_digest: "+err.Error())
			return
		}
		req.ToDigest = digest
	}

	result, err := a.rollbacks.Rollback(r.Context(), req)
	if err != nil {
		a.rollbackError(w, r, err)
		return
	}
	resp := rollbackResponse{
		Deployment: newDeployment(result.Rollback),
		RolledBack: newDeployment(result.RolledBack),
		Cancelled:  result.Cancelled,
		Migrations: newDeployments(result.Migrations),
	}
	if resp.Cancelled == nil {
		resp.Cancelled = []core.DeploymentID{}
	}
	if result.Block != nil {
		b := newBlockResponse(*result.Block)
		resp.Block = &b
	}
	w.Header().Set("Location", Prefix+"deployments/"+strconv.FormatInt(int64(result.Rollback.ID), 10))
	writeJSON(w, http.StatusCreated, resp)
}

func (a *api) rollbackError(w http.ResponseWriter, r *http.Request, err error) {
	if inProgress, ok := errors.AsType[*core.RollbackInProgressError](err); ok {
		writeJSON(w, http.StatusConflict, rollbackInProgressResponse{
			Error:      errorBody{Code: codeRollbackInProgress, Message: inProgress.Error()},
			Deployment: newDeployment(inProgress.Deployment),
		})
		return
	}
	if crossing, ok := errors.AsType[*core.MigrationRollbackError](err); ok {
		writeJSON(w, http.StatusConflict, migrationConfirmationResponse{
			Error:      errorBody{Code: codeMigrationConfirmationRequired, Message: crossing.Error()},
			To:         newDeployment(crossing.To),
			Migrations: newDeployments(crossing.Migrations),
		})
		return
	}
	if blocked, ok := errors.AsType[*core.BlockedError](err); ok {
		writeJSON(w, http.StatusConflict, blockedResponse{
			Error: errorBody{Code: codeBlocked, Message: blocked.Error()},
			Block: newBlockResponse(blocked.Block),
		})
		return
	}
	switch {
	case errors.Is(err, core.ErrInvalidRollback):
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
	case errors.Is(err, core.ErrNoRollbackTarget):
		writeError(w, http.StatusConflict, codeNoRollbackTarget, err.Error())
	case errors.Is(err, core.ErrStateConflict):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, codeContended, "deployments changed while the rollback was being requested; retry")
	default:
		a.internalError(w, r, err)
	}
}
