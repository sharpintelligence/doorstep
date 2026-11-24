package web

import (
	"fmt"
	"net/http"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type lockForm struct {
	Reason    string
	ExpiresIn string
}

type locksView struct {
	Global *core.Lock
	Apps   []core.Lock
	Events []core.LockEvent
	Form   lockForm
}

func (h *Handler) locksPage(w http.ResponseWriter, r *http.Request) {
	h.renderLocks(w, r, http.StatusOK, "", lockForm{})
}

func (h *Handler) renderLocks(w http.ResponseWriter, r *http.Request, status int, message string, form lockForm) {
	ctx := r.Context()
	locks, err := h.locks.List(ctx)
	if err != nil {
		h.internalError(w, r, fmt.Errorf("list locks: %w", err))
		return
	}
	view := locksView{Form: form}
	for _, l := range locks {
		if l.Global {
			view.Global = &l
			continue
		}
		view.Apps = append(view.Apps, l)
	}
	if view.Events, err = h.lockEvents.ListLockEvents(ctx, lockEventsMax); err != nil {
		h.internalError(w, r, fmt.Errorf("list lock events: %w", err))
		return
	}
	p := h.newPage(r, "Locks", "locks", view)
	p.Error = message
	p.Live = "/locks"
	h.render(w, r, status, "locks", p)
}
