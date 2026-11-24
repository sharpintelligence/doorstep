package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type appForm struct {
	CommitSHA      string
	Urgent         bool
	DeployReason   string
	RollbackReason string
	LockReason     string
	LockExpiresIn  string
}

type appView struct {
	App        App
	Current    *core.Deployment
	Queue      []core.QueueEntry
	History    []core.Deployment
	Before     core.DeploymentID
	Next       core.DeploymentID
	Blocks     []core.Block
	Lock       *core.Lock
	GlobalLock *core.Lock
	CanDeploy  bool
	Form       appForm
}

func (v appView) NextURL() string {
	return "/apps/" + url.PathEscape(v.App.Name) + "?before=" + strconv.FormatInt(int64(v.Next), 10)
}

func (h *Handler) lookupApp(w http.ResponseWriter, r *http.Request) (App, bool) {
	app, ok := h.byName[r.PathValue("app")]
	if !ok {
		h.renderError(w, r, http.StatusNotFound, "Unknown app", "No app with this name is configured.")
	}
	return app, ok
}

func (h *Handler) appPage(w http.ResponseWriter, r *http.Request) {
	app, ok := h.lookupApp(w, r)
	if !ok {
		return
	}
	var before core.DeploymentID
	if raw := r.URL.Query().Get("before"); raw != "" {
		id, err := parseID(raw)
		if err != nil {
			h.renderError(w, r, http.StatusBadRequest, "Bad page", "The page cursor must be a deployment id.")
			return
		}
		before = id
	}
	h.renderApp(w, r, app, before, http.StatusOK, "", appForm{})
}

func (h *Handler) renderApp(w http.ResponseWriter, r *http.Request, app App, before core.DeploymentID, status int, message string, form appForm) {
	view, err := h.loadApp(r.Context(), app, before)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	view.Form = form
	p := h.newPage(r, app.Name, "apps", view)
	view.CanDeploy = h.deploys != nil && p.CanOperate()
	p.Content = view
	p.Error = message
	p.Live = "/apps/" + url.PathEscape(app.Name)
	if before > 0 {
		p.Live += "?before=" + strconv.FormatInt(int64(before), 10)
	}
	h.render(w, r, status, "app", p)
}

func (h *Handler) loadApp(ctx context.Context, app App, before core.DeploymentID) (appView, error) {
	view := appView{App: app, Before: before}
	var err error
	if view.Current, err = h.current(ctx, app.Target); err != nil {
		return appView{}, err
	}
	entries, err := h.queue.Entries(ctx, app.Name)
	if err != nil {
		return appView{}, fmt.Errorf("queue of %s: %w", app.Name, err)
	}
	for _, e := range entries {
		if e.Deployment.Target == app.Target {
			view.Queue = append(view.Queue, e)
		}
	}
	page, err := h.history.ListDeployments(ctx, store.DeploymentFilter{
		App: app.Target.App, Environment: app.Target.Environment, Before: before, Limit: historyLimit,
	})
	if err != nil {
		return appView{}, fmt.Errorf("history of %s: %w", app.Target, err)
	}
	view.History, view.Next = page.Items, page.Next
	if view.Blocks, err = h.blocks.ListBlocks(ctx, app.Target); err != nil {
		return appView{}, fmt.Errorf("blocks of %s: %w", app.Target, err)
	}
	locks, err := h.readLocks(ctx)
	if err != nil {
		return appView{}, err
	}
	view.Lock, view.GlobalLock = locks.apps[app.Name], locks.global
	return view, nil
}

func parseID(raw string) (core.DeploymentID, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		return 0, fmt.Errorf("invalid id %q", raw)
	}
	return core.DeploymentID(id), nil
}
