package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type appSummary struct {
	App        App
	Current    *core.Deployment
	Latest     *core.Deployment
	QueueDepth int
	InFlight   int
	Lock       *core.Lock
}

type overviewView struct {
	Apps       []appSummary
	GlobalLock *core.Lock
}

type lockSet struct {
	global *core.Lock
	apps   map[string]*core.Lock
}

func (h *Handler) readLocks(ctx context.Context) (lockSet, error) {
	locks, err := h.locks.List(ctx)
	if err != nil {
		return lockSet{}, fmt.Errorf("list locks: %w", err)
	}
	set := lockSet{apps: map[string]*core.Lock{}}
	for _, l := range locks {
		if l.Global {
			set.global = &l
			continue
		}
		set.apps[l.Target.App] = &l
	}
	return set, nil
}

func (h *Handler) current(ctx context.Context, t core.Target) (*core.Deployment, error) {
	d, err := h.history.CurrentDeployment(ctx, t)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("current deployment of %s: %w", t, err)
	}
	return &d, nil
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	locks, err := h.readLocks(ctx)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	view := overviewView{GlobalLock: locks.global}
	for _, app := range h.apps {
		summary, err := h.summarize(ctx, app)
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		summary.Lock = locks.apps[app.Name]
		view.Apps = append(view.Apps, summary)
	}
	p := h.newPage(r, "Apps", "apps", view)
	p.Live = "/"
	h.render(w, r, http.StatusOK, "overview", p)
}

func (h *Handler) summarize(ctx context.Context, app App) (appSummary, error) {
	s := appSummary{App: app}
	var err error
	if s.Current, err = h.current(ctx, app.Target); err != nil {
		return appSummary{}, err
	}
	page, err := h.history.ListDeployments(ctx, store.DeploymentFilter{App: app.Target.App, Environment: app.Target.Environment, Limit: 1})
	if err != nil {
		return appSummary{}, fmt.Errorf("latest deployment of %s: %w", app.Target, err)
	}
	if len(page.Items) > 0 {
		s.Latest = &page.Items[0]
	}
	entries, err := h.queue.Entries(ctx, app.Name)
	if err != nil {
		return appSummary{}, fmt.Errorf("queue of %s: %w", app.Name, err)
	}
	for _, e := range entries {
		if e.Deployment.Target != app.Target {
			continue
		}
		if e.InFlight() {
			s.InFlight++
		} else {
			s.QueueDepth++
		}
	}
	return s, nil
}
