package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sharpintelligence/doorstep/internal/core"
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
	view, err := h.loadOverview(r.Context())
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	p := h.newPage(r, "Apps", "apps", view)
	p.Live = "/"
	h.render(w, r, http.StatusOK, "overview", p)
}

func byTarget(deployments []core.Deployment) map[core.Target]*core.Deployment {
	out := make(map[core.Target]*core.Deployment, len(deployments))
	for i := range deployments {
		out[deployments[i].Target] = &deployments[i]
	}
	return out
}

func (h *Handler) loadOverview(ctx context.Context) (overviewView, error) {
	locks, err := h.readLocks(ctx)
	if err != nil {
		return overviewView{}, err
	}
	current, err := h.history.CurrentDeployments(ctx)
	if err != nil {
		return overviewView{}, fmt.Errorf("current deployments: %w", err)
	}
	latest, err := h.history.LatestDeployments(ctx)
	if err != nil {
		return overviewView{}, fmt.Errorf("latest deployments: %w", err)
	}
	entries, err := h.queue.Entries(ctx, "")
	if err != nil {
		return overviewView{}, fmt.Errorf("queue: %w", err)
	}
	currentByTarget, latestByTarget := byTarget(current), byTarget(latest)
	waiting, inFlight := map[core.Target]int{}, map[core.Target]int{}
	for _, e := range entries {
		if e.InFlight() {
			inFlight[e.Deployment.Target]++
		} else {
			waiting[e.Deployment.Target]++
		}
	}
	view := overviewView{GlobalLock: locks.global, Apps: make([]appSummary, 0, len(h.apps))}
	for _, app := range h.apps {
		view.Apps = append(view.Apps, appSummary{
			App:        app,
			Current:    currentByTarget[app.Target],
			Latest:     latestByTarget[app.Target],
			QueueDepth: waiting[app.Target],
			InFlight:   inFlight[app.Target],
			Lock:       locks.apps[app.Name],
		})
	}
	return view, nil
}
