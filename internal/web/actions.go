package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/locks"
	"github.com/sharpintelligence/doorstep/internal/rollback"
)

type confirmMigrationView struct {
	App            App
	To             core.Deployment
	Migrations     []core.Deployment
	ToDeploymentID string
	Reason         string
}

func (h *Handler) formPrincipal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, auth.MaxFormBytes)
	if err := r.ParseForm(); err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			h.renderError(w, r, http.StatusRequestEntityTooLarge, "Form too large", "The form was larger than doorstep accepts.")
			return auth.Principal{}, false
		}
		h.renderError(w, r, http.StatusBadRequest, "Bad form", "The form could not be read.")
		return auth.Principal{}, false
	}
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		h.renderError(w, r, http.StatusUnauthorized, "Sign-in required", "Sign in to continue.")
	}
	return p, ok
}

func appPath(app App) string {
	return "/apps/" + url.PathEscape(app.Name)
}

func deploymentPath(id core.DeploymentID) string {
	return "/deployments/" + strconv.FormatInt(int64(id), 10)
}

func (h *Handler) deploy(w http.ResponseWriter, r *http.Request) {
	app, ok := h.lookupApp(w, r)
	if !ok {
		return
	}
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	form := appForm{
		CommitSHA:    strings.TrimSpace(r.PostFormValue("commit_sha")),
		Urgent:       r.PostFormValue("urgent") != "",
		DeployReason: r.PostFormValue("reason"),
	}
	result, err := h.deploys.Deploy(r.Context(), deploys.Request{
		Target:    app.Target,
		CommitSHA: form.CommitSHA,
		Urgent:    form.Urgent,
		Reason:    form.DeployReason,
		Actor:     principal.Actor,
		Role:      principal.Role,
	})
	if err == nil {
		notice := "deploy_queued"
		if !result.Created {
			notice = "deploy_exists"
		}
		seeOther(w, deploymentPath(result.Deployment.ID), notice)
		return
	}
	status, message := deployError(err, app)
	if status == http.StatusInternalServerError {
		h.internalError(w, r, err)
		return
	}
	h.renderApp(w, r, app, 0, status, message, form)
}

func deployError(err error, app App) (int, string) {
	if blocked, ok := errors.AsType[*core.BlockedError](err); ok {
		return http.StatusConflict, "Not deployed: " + blocked.Error() + ". Remove the block first if the version is safe again."
	}
	switch {
	case errors.Is(err, deploys.ErrInvalidRequest):
		return http.StatusBadRequest, "Not deployed: the commit must be a full 40 character SHA and the reason at most 500 characters."
	case errors.Is(err, deploys.ErrUnknownApp):
		return http.StatusNotFound, "Not deployed: " + app.Name + " is not configured."
	case errors.Is(err, deploys.ErrCommitNotFound):
		return http.StatusNotFound, "Not deployed: the commit does not exist in " + app.Repository + "."
	case errors.Is(err, deploys.ErrCommitNotOnBranch):
		return http.StatusConflict, "Not deployed: the commit is not on " + app.Branch + " of " + app.Repository + ". Only merged commits can be deployed."
	case errors.Is(err, deploys.ErrUnavailable):
		return http.StatusServiceUnavailable, "Not deployed: GitHub or migration detection is unavailable. Try again in a moment."
	case errors.Is(err, deploys.ErrNotCancellable):
		return http.StatusConflict, "The deployment has already finished, so there is nothing to cancel."
	case errors.Is(err, core.ErrStateConflict):
		return http.StatusServiceUnavailable, "The deployment changed while it was being cancelled. Try again."
	default:
		return http.StatusInternalServerError, ""
	}
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, http.StatusNotFound, "Unknown deployment", "Deployment ids are positive whole numbers.")
		return
	}
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	reason := r.PostFormValue("reason")
	_, err = h.deploys.Cancel(r.Context(), deploys.CancelRequest{ID: id, Reason: reason, Actor: principal.Actor, Role: principal.Role})
	switch {
	case err == nil:
		seeOther(w, deploymentPath(id), "deploy_cancelled")
		return
	case errors.Is(err, core.ErrNotFound):
		h.renderError(w, r, http.StatusNotFound, "Unknown deployment", "Deployment "+strconv.FormatInt(int64(id), 10)+" does not exist.")
		return
	case errors.Is(err, deploys.ErrInvalidRequest):
		h.renderDeployment(w, r, id, http.StatusBadRequest, "Not cancelled: the reason must be at most 500 characters.", reason)
		return
	}
	status, message := deployError(err, App{})
	if status == http.StatusInternalServerError {
		h.internalError(w, r, err)
		return
	}
	h.renderDeployment(w, r, id, status, message, reason)
}

func (h *Handler) rollback(w http.ResponseWriter, r *http.Request) {
	app, ok := h.lookupApp(w, r)
	if !ok {
		return
	}
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	form := appForm{RollbackReason: r.PostFormValue("reason")}
	req := rollback.Request{
		Target:           app.Target,
		Reason:           form.RollbackReason,
		ConfirmMigration: r.PostFormValue("confirm_migration_rollback") == "1",
		Actor:            principal.Actor,
		Role:             principal.Role,
	}
	toRaw := strings.TrimSpace(r.PostFormValue("to_deployment_id"))
	if toRaw != "" {
		to, err := parseID(toRaw)
		if err != nil {
			h.renderApp(w, r, app, 0, http.StatusBadRequest, "Not rolled back: the deployment to roll back to must be a deployment id.", form)
			return
		}
		req.ToDeployment = &to
	}
	result, err := h.rollbacks.Rollback(r.Context(), req)
	if err == nil {
		seeOther(w, deploymentPath(result.Rollback.ID), "rollback_queued")
		return
	}
	if crossing, ok := errors.AsType[*core.MigrationRollbackError](err); ok {
		view := confirmMigrationView{App: app, To: crossing.To, Migrations: crossing.Migrations, ToDeploymentID: toRaw, Reason: form.RollbackReason}
		h.render(w, r, http.StatusConflict, "confirm_migration", h.newPage(r, "Confirm rollback", "apps", view))
		return
	}
	status, message := rollbackError(err)
	if status == http.StatusInternalServerError {
		h.internalError(w, r, err)
		return
	}
	h.renderApp(w, r, app, 0, status, message, form)
}

func rollbackError(err error) (int, string) {
	if inProgress, ok := errors.AsType[*core.RollbackInProgressError](err); ok {
		return http.StatusConflict, "Not rolled back: " + inProgress.Error() + "."
	}
	if blocked, ok := errors.AsType[*core.BlockedError](err); ok {
		return http.StatusConflict, "Not rolled back: the version to roll back to is blocked: " + blocked.Error() + "."
	}
	switch {
	case errors.Is(err, core.ErrNoRollbackTarget):
		return http.StatusConflict, "Not rolled back: there is no earlier successful, unblocked version to roll back to."
	case errors.Is(err, core.ErrInvalidRollback):
		return http.StatusBadRequest, "Not rolled back: " + err.Error()
	case errors.Is(err, core.ErrStateConflict):
		return http.StatusServiceUnavailable, "Not rolled back: deployments changed while the rollback was requested. Try again."
	default:
		return http.StatusInternalServerError, ""
	}
}

func (h *Handler) block(w http.ResponseWriter, r *http.Request) {
	app, ok := h.lookupApp(w, r)
	if !ok {
		return
	}
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	values := appForm{BlockRef: r.PostFormValue("ref"), BlockReason: r.PostFormValue("reason")}
	ref, err := core.ParseBlockRef(strings.ToLower(strings.TrimSpace(values.BlockRef)))
	if err != nil {
		h.renderApp(w, r, app, 0, http.StatusBadRequest, "Not blocked: name a sha256 digest or a full commit SHA.", values)
		return
	}
	_, created, err := h.blocks.Block(r.Context(), rollback.BlockRequest{
		Target: app.Target,
		Ref:    ref,
		Reason: values.BlockReason,
		Actor:  principal.Actor,
		Role:   principal.Role,
	})
	switch {
	case errors.Is(err, core.ErrInvalidBlock):
		h.renderApp(w, r, app, 0, http.StatusBadRequest, "Not blocked: "+err.Error(), values)
	case err != nil:
		h.internalError(w, r, err)
	case created:
		seeOther(w, appPath(app), "blocked")
	default:
		seeOther(w, appPath(app), "block_exists")
	}
}

func (h *Handler) unblock(w http.ResponseWriter, r *http.Request) {
	app, ok := h.lookupApp(w, r)
	if !ok {
		return
	}
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	ref, err := core.ParseBlockRef(strings.TrimSpace(r.PostFormValue("ref")))
	if err != nil {
		h.renderApp(w, r, app, 0, http.StatusBadRequest, "Not unblocked: name a sha256 digest or a full commit SHA.", appForm{})
		return
	}
	_, err = h.blocks.Unblock(r.Context(), app.Target, ref, principal.Actor, principal.Role)
	switch {
	case err == nil:
		seeOther(w, appPath(app), "unblocked")
	case errors.Is(err, core.ErrNotFound):
		h.renderApp(w, r, app, 0, http.StatusNotFound, "Not unblocked: "+ref.String()+" is not blocked for "+app.Name+".", appForm{})
	case errors.Is(err, core.ErrInvalidBlock):
		h.renderApp(w, r, app, 0, http.StatusBadRequest, "Not unblocked: "+err.Error(), appForm{})
	default:
		h.internalError(w, r, err)
	}
}

func lockRequest(r *http.Request, principal auth.Principal, target *core.Target) (locks.Request, lockForm, string) {
	form := lockForm{Reason: r.PostFormValue("reason"), ExpiresIn: strings.TrimSpace(r.PostFormValue("expires_in"))}
	req := locks.Request{Target: target, Reason: form.Reason, Actor: principal.Actor, Role: principal.Role}
	if form.ExpiresIn != "" {
		d, err := time.ParseDuration(form.ExpiresIn)
		if err != nil {
			return locks.Request{}, form, `Not locked: the expiry must be a duration with a unit, such as "2h" or "30m".`
		}
		req.ExpiresIn = &d
	}
	return req, form, ""
}

func lockError(err error) (int, string) {
	if held, ok := errors.AsType[*core.LockHeldError](err); ok {
		return http.StatusConflict, "Not locked: " + held.Error()
	}
	switch {
	case errors.Is(err, core.ErrInvalidLock):
		return http.StatusBadRequest, "Not locked: " + err.Error()
	case errors.Is(err, core.ErrLockContended):
		return http.StatusServiceUnavailable, "Not locked: the lock changed while it was being taken. Try again."
	default:
		return http.StatusInternalServerError, ""
	}
}

func (h *Handler) lockApp(w http.ResponseWriter, r *http.Request) {
	app, ok := h.lookupApp(w, r)
	if !ok {
		return
	}
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	target := app.Target
	req, form, message := lockRequest(r, principal, &target)
	values := appForm{LockReason: form.Reason, LockExpiresIn: form.ExpiresIn}
	if message != "" {
		h.renderApp(w, r, app, 0, http.StatusBadRequest, message, values)
		return
	}
	if _, err := h.locks.Lock(r.Context(), req); err != nil {
		status, message := lockError(err)
		if status == http.StatusInternalServerError {
			h.internalError(w, r, err)
			return
		}
		h.renderApp(w, r, app, 0, status, message, values)
		return
	}
	seeOther(w, appPath(app), "locked")
}

func (h *Handler) unlockApp(w http.ResponseWriter, r *http.Request) {
	app, ok := h.lookupApp(w, r)
	if !ok {
		return
	}
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	target := app.Target
	_, err := h.locks.Unlock(r.Context(), &target, principal.Actor, principal.Role)
	switch {
	case err == nil:
		seeOther(w, appPath(app), "unlocked")
	case errors.Is(err, core.ErrNotFound):
		h.renderApp(w, r, app, 0, http.StatusNotFound, "Not unlocked: "+app.Name+" is not locked.", appForm{})
	default:
		h.internalError(w, r, err)
	}
}

func (h *Handler) lockGlobal(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	req, form, message := lockRequest(r, principal, nil)
	if message != "" {
		h.renderLocks(w, r, http.StatusBadRequest, message, form)
		return
	}
	if _, err := h.locks.Lock(r.Context(), req); err != nil {
		status, message := lockError(err)
		if status == http.StatusInternalServerError {
			h.internalError(w, r, err)
			return
		}
		h.renderLocks(w, r, status, message, form)
		return
	}
	seeOther(w, "/locks", "global_locked")
}

func (h *Handler) unlockGlobal(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	_, err := h.locks.Unlock(r.Context(), nil, principal.Actor, principal.Role)
	switch {
	case err == nil:
		seeOther(w, "/locks", "global_unlocked")
	case errors.Is(err, core.ErrNotFound):
		h.renderLocks(w, r, http.StatusNotFound, "Not unlocked: there is no global lock.", lockForm{})
	default:
		h.internalError(w, r, err)
	}
}
