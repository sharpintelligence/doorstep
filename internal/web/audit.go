package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type auditView struct {
	Entries []core.AuditEntry
	Actions []core.AuditAction
	Apps    []App
	Action  core.AuditAction
	App     string
	Before  int64
	Next    int64
}

func (v auditView) NextURL() string {
	q := url.Values{"before": {strconv.FormatInt(v.Next, 10)}}
	if v.Action != "" {
		q.Set("action", string(v.Action))
	}
	if v.App != "" {
		q.Set("app", v.App)
	}
	return "/audit?" + q.Encode()
}

func (h *Handler) auditPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	view := auditView{Actions: core.AuditActions(), Apps: h.apps, Action: core.AuditAction(q.Get("action")), App: q.Get("app")}
	filter := store.AuditFilter{Limit: auditLimit}
	if view.Action != "" {
		if !view.Action.Valid() {
			h.renderError(w, r, http.StatusBadRequest, "Bad filter", "The action filter must be an audit action such as deploy.request.")
			return
		}
		filter.Action = view.Action
	}
	if view.App != "" {
		app, ok := h.byName[view.App]
		if !ok {
			h.renderError(w, r, http.StatusBadRequest, "Bad filter", "The app filter must name a configured app.")
			return
		}
		filter.Target = &app.Target
	}
	if raw := q.Get("before"); raw != "" {
		before, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || before <= 0 {
			h.renderError(w, r, http.StatusBadRequest, "Bad page", "The page cursor must be an audit entry id.")
			return
		}
		view.Before, filter.BeforeID = before, before
	}
	entries, err := h.audit.ListAudit(r.Context(), filter)
	if err != nil {
		h.internalError(w, r, fmt.Errorf("list audit: %w", err))
		return
	}
	view.Entries = entries
	if len(entries) == auditLimit {
		view.Next = entries[len(entries)-1].ID
	}
	h.render(w, r, http.StatusOK, "audit", h.newPage(r, "Audit log", "audit", view))
}
