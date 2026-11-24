package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type gatePhase struct {
	Phase   core.GatePhase
	Results []core.GateResult
}

type deploymentView struct {
	store.DeploymentHistory
	App          App
	Configured   bool
	Phases       []gatePhase
	Attempts     []attemptView
	CanCancel    bool
	CancelReason string
}

func (h *Handler) deploymentPage(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, http.StatusNotFound, "Unknown deployment", "Deployment ids are positive whole numbers.")
		return
	}
	h.renderDeployment(w, r, id, http.StatusOK, "", "")
}

func (h *Handler) renderDeployment(w http.ResponseWriter, r *http.Request, id core.DeploymentID, status int, message, reason string) {
	history, err := h.history.GetDeploymentHistory(r.Context(), id)
	if errors.Is(err, core.ErrNotFound) {
		h.renderError(w, r, http.StatusNotFound, "Unknown deployment", fmt.Sprintf("Deployment %d does not exist.", id))
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	d := history.Deployment
	view := deploymentView{DeploymentHistory: history, CancelReason: reason}
	view.App, view.Configured = h.byName[d.Target.App]
	view.Configured = view.Configured && view.App.Target == d.Target
	for _, phase := range core.GatePhases() {
		var results []core.GateResult
		for _, g := range history.Gates {
			if g.Phase == phase {
				results = append(results, g)
			}
		}
		if len(results) > 0 {
			view.Phases = append(view.Phases, gatePhase{Phase: phase, Results: results})
		}
	}
	view.Attempts = attemptsFrom(history.Events)
	p := h.newPage(r, "Deployment "+strconv.FormatInt(int64(d.ID), 10), "apps", nil)
	view.CanCancel = h.deploys != nil && p.CanOperate() && !d.State.Terminal()
	p.Content = view
	p.Error = message
	if !d.State.Terminal() {
		p.Live = "/deployments/" + strconv.FormatInt(int64(d.ID), 10)
	}
	h.render(w, r, status, "deployment", p)
}
