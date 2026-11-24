package web

import (
	"log/slog"
	"net/http"
	"net/url"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	headerHXRequest    = "HX-Request"
	headerHXRedirect   = "HX-Redirect"
	headerHXCurrentURL = "HX-Current-URL"
)

func (h *Handler) guard(rt route) http.Handler {
	switch {
	case rt.asset:
		return rt.handler
	case rt.role == "":
		return withSecurityHeaders(rt.handler)
	}
	return h.anonymous(h.requireLogin(h.allow(rt.role, withSecurityHeaders(rt.handler))))
}

func setSecurityHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "same-origin")
	header.Set("Cache-Control", "no-store")
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) anonymous(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.PrincipalFrom(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case r.Header.Get(headerHXRequest) == "true":
			login := "/"
			if h.signIn {
				login = auth.LoginPath + "?return_to=" + url.QueryEscape(htmxReturnPath(r))
			}
			w.Header().Set(headerHXRedirect, login)
			h.renderError(w, r, http.StatusUnauthorized, "Signed out", "Your session has ended. Sign in again to continue.")
		case !h.signIn:
			h.renderError(w, r, http.StatusUnauthorized, "Sign-in required",
				"The doorstep web UI needs a sign-in provider. Ask an administrator to configure OpenID Connect or GitHub sign-in; API tokens work only with the API and CLI.")
		case r.Method != http.MethodGet && r.Method != http.MethodHead:
			h.renderError(w, r, http.StatusUnauthorized, "Signed out", "Your session has ended, so nothing was changed. Sign in again and retry.")
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func htmxReturnPath(r *http.Request) string {
	if current, err := url.Parse(r.Header.Get(headerHXCurrentURL)); err == nil && current.Path != "" {
		return auth.SafeReturnPath(current.RequestURI())
	}
	return auth.SafeReturnPath(r.URL.RequestURI())
}

func (h *Handler) allow(role core.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok {
			h.renderError(w, r, http.StatusUnauthorized, "Sign-in required", "Sign in to continue.")
			return
		}
		if !p.Allows(role) {
			h.logger.WarnContext(r.Context(), "authorization denied",
				slog.String("method", r.Method), slog.String("path", r.URL.Path),
				slog.String("actor", p.Actor.String()), slog.String("role", string(p.Role)), slog.String("required", string(role)))
			h.renderError(w, r, http.StatusForbidden, "Not allowed",
				"Your role ("+string(p.Role)+") does not allow this. It needs the "+string(role)+" role.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
