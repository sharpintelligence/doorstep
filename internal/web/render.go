package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/core"
)

//go:embed templates/*.html
var templateFS embed.FS

var pageNames = []string{"overview", "app", "deployment", "locks", "audit", "tokens", "token_created", "confirm_migration", "error"}

var notices = map[string]string{
	"deploy_queued":    "Deploy queued.",
	"deploy_exists":    "A deploy of this commit was already queued or running; this is that deployment.",
	"deploy_cancelled": "Deployment cancelled.",
	"rollback_queued":  "Rollback queued. The version it replaces is blocked from deploying again automatically.",
	"blocked":          "Version blocked. It will not be deployed automatically until it is unblocked.",
	"block_exists":     "This version was already blocked.",
	"unblocked":        "Block removed.",
	"locked":           "App locked.",
	"unlocked":         "App unlocked.",
	"global_locked":    "All apps locked.",
	"global_unlocked":  "Global lock released.",
	"token_revoked":    "Token revoked.",
}

type page struct {
	Title     string
	Nav       string
	Principal auth.Principal
	SignedIn  bool
	CSRF      string
	Notice    string
	Error     string
	Live      string
	Build     buildinfo.Info
	Content   any
}

func (p page) IsAdmin() bool {
	return p.SignedIn && p.Principal.Allows(core.RoleAdmin)
}

func (p page) CanOperate() bool {
	return p.SignedIn && p.Principal.Allows(core.RoleDeployer)
}

func (p page) Session() bool {
	return p.SignedIn && p.Principal.Method == auth.MethodSession
}

func (p page) ActorName() string {
	if p.Principal.Actor.Name != "" {
		return p.Principal.Actor.Name
	}
	return p.Principal.Actor.String()
}

func (h *Handler) parseTemplates() (map[string]*template.Template, error) {
	base, err := template.New("layout.html").Funcs(h.funcs()).ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse layout: %w", err)
	}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		clone, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("web: clone layout for %s: %w", name, err)
		}
		if pages[name], err = clone.ParseFS(templateFS, "templates/"+name+".html"); err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", name, err)
		}
	}
	return pages, nil
}

func (h *Handler) newPage(r *http.Request, title, nav string, content any) page {
	p := page{Title: title, Nav: nav, Build: h.build, Content: content}
	if principal, ok := auth.PrincipalFrom(r.Context()); ok {
		p.Principal, p.SignedIn = principal, true
		p.CSRF = auth.CSRFToken(r.Context())
	}
	if msg, ok := notices[r.URL.Query().Get("notice")]; ok && r.Method == http.MethodGet {
		p.Notice = msg
	}
	return p
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, name string, p page) {
	var buf bytes.Buffer
	if err := h.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		h.logger.ErrorContext(r.Context(), "rendering page failed", slog.String("page", name), slog.String("error", err.Error()))
		setSecurityHeaders(w)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = buf.WriteTo(w)
	}
}

type errorView struct {
	Status  int
	Heading string
	Message string
	SignIn  string
}

func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	view := errorView{Status: status, Heading: heading, Message: message}
	if status == http.StatusUnauthorized && h.signIn {
		view.SignIn = auth.LoginPath + "?return_to=" + url.QueryEscape(auth.SafeReturnPath(returnPathFor(r)))
	}
	h.render(w, r, status, "error", h.newPage(r, heading, "", view))
}

func returnPathFor(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return r.URL.RequestURI()
	}
	return r.Header.Get("Referer")
}

func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.ErrorContext(r.Context(), "web request failed",
		slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("error", err.Error()))
	h.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "doorstep could not complete this request. The error was logged; try again.")
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "Not found", "There is no page at this address.")
}

func seeOther(w http.ResponseWriter, path, notice string) {
	target := auth.SafeReturnPath(path)
	if notice != "" {
		target += "?notice=" + url.QueryEscape(notice)
	}
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusSeeOther)
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
