package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/rollback"
)

const (
	appBlocksPath = Prefix + "apps/{app}/blocks"
	appBlockPath  = Prefix + "apps/{app}/blocks/{ref}"
)

type blockRequest struct {
	Digest    string `json:"digest"`
	CommitSHA string `json:"commit_sha"`
	Reason    string `json:"reason"`
}

type blockCommit struct {
	Repository string `json:"repository,omitempty"`
	SHA        string `json:"sha"`
}

type blockResponse struct {
	ID            core.BlockID       `json:"id"`
	App           string             `json:"app"`
	Environment   string             `json:"environment"`
	Digest        core.Digest        `json:"digest,omitempty"`
	Commit        *blockCommit       `json:"commit,omitempty"`
	Reason        string             `json:"reason"`
	CreatedBy     string             `json:"created_by"`
	DeploymentID  *core.DeploymentID `json:"deployment_id,omitempty"`
	RollbackID    *core.DeploymentID `json:"rollback_id,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	RecordedInGit bool               `json:"recorded_in_git"`
	ClearedAt     *time.Time         `json:"cleared_at,omitempty"`
	ClearedBy     string             `json:"cleared_by,omitempty"`
}

type blockListResponse struct {
	Blocks []blockResponse `json:"blocks"`
}

func (a *api) registerBlocks(mux *http.ServeMux) {
	mux.Handle("GET "+appBlocksPath, a.authenticated(core.RoleViewer, a.listBlocks))
	mux.Handle("POST "+appBlocksPath, a.authenticated(core.RoleDeployer, a.createBlock))
	mux.HandleFunc(appBlocksPath, methodNotAllowed(http.MethodGet, http.MethodHead, http.MethodPost))
	mux.Handle("DELETE "+appBlockPath, a.authenticated(core.RoleDeployer, a.clearBlock))
	mux.HandleFunc(appBlockPath, methodNotAllowed(http.MethodDelete))
}

func newBlockResponse(b core.Block) blockResponse {
	resp := blockResponse{
		ID:            b.ID,
		App:           b.Target.App,
		Environment:   b.Target.Environment,
		Digest:        b.Digest,
		Reason:        b.Reason,
		CreatedBy:     b.CreatedBy,
		DeploymentID:  b.DeploymentID,
		RollbackID:    b.RollbackID,
		CreatedAt:     b.CreatedAt.UTC(),
		RecordedInGit: b.BlockCommitSHA != "",
		ClearedAt:     utc(b.ClearedAt),
		ClearedBy:     b.ClearedBy,
	}
	if b.Commit.SHA != "" {
		resp.Commit = &blockCommit{Repository: b.Commit.Repository, SHA: b.Commit.SHA}
	}
	return resp
}

func newBlockList(blocks []core.Block) blockListResponse {
	resp := blockListResponse{Blocks: make([]blockResponse, 0, len(blocks))}
	for _, b := range blocks {
		resp.Blocks = append(resp.Blocks, newBlockResponse(b))
	}
	return resp
}

func (a *api) listBlocks(w http.ResponseWriter, r *http.Request, _ Caller) {
	app := r.PathValue("app")
	if !a.known(app) {
		writeError(w, http.StatusNotFound, codeNotFound, "unknown app "+app)
		return
	}
	blocks, err := a.blocks.ListBlocks(r.Context(), core.AppTarget(app))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newBlockList(blocks))
}

func (a *api) createBlock(w http.ResponseWriter, r *http.Request, caller Caller) {
	app := r.PathValue("app")
	if !a.known(app) {
		writeError(w, http.StatusNotFound, codeNotFound, "unknown app "+app)
		return
	}
	var body blockRequest
	if reqErr := decodeJSON(w, r, &body); reqErr != nil {
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
		return
	}
	ref := core.BlockRef{Digest: core.Digest(body.Digest), CommitSHA: body.CommitSHA}
	if err := ref.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, "set digest, commit_sha or both: "+err.Error())
		return
	}
	block, created, err := a.blocks.Block(r.Context(), rollback.BlockRequest{
		Target: core.AppTarget(app),
		Ref:    ref,
		Reason: body.Reason,
		Actor:  caller.Actor,
		Role:   caller.Role,
	})
	if errors.Is(err, core.ErrInvalidBlock) {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, newBlockResponse(block))
}

func (a *api) clearBlock(w http.ResponseWriter, r *http.Request, caller Caller) {
	ref, err := core.ParseBlockRef(r.PathValue("ref"))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	target := core.AppTarget(r.PathValue("app"))
	cleared, err := a.blocks.Unblock(r.Context(), target, ref, caller.Actor, caller.Role)
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, ref.String()+" is not blocked for "+target.App)
	case errors.Is(err, core.ErrInvalidBlock):
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error())
	case err != nil:
		a.internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, newBlockList(cleared))
	}
}
