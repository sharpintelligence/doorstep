package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type tokenForm struct {
	Name      string
	Role      core.Role
	ExpiresIn string
}

type tokensView struct {
	Tokens []store.APIToken
	Roles  []core.Role
	Form   tokenForm
}

type tokenCreatedView struct {
	Raw   string
	Token store.APIToken
}

func (h *Handler) tokensPage(w http.ResponseWriter, r *http.Request) {
	h.renderTokens(w, r, http.StatusOK, "", tokenForm{Role: core.RoleViewer})
}

func (h *Handler) renderTokens(w http.ResponseWriter, r *http.Request, status int, message string, form tokenForm) {
	tokens, err := h.tokens.List(r.Context())
	if err != nil {
		h.internalError(w, r, fmt.Errorf("list api tokens: %w", err))
		return
	}
	p := h.newPage(r, "API tokens", "tokens", tokensView{Tokens: tokens, Roles: core.Roles(), Form: form})
	p.Error = message
	h.render(w, r, status, "tokens", p)
}

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	form := tokenForm{
		Name:      strings.TrimSpace(r.PostFormValue("name")),
		Role:      core.Role(r.PostFormValue("role")),
		ExpiresIn: strings.TrimSpace(r.PostFormValue("expires_in")),
	}
	var expiresIn time.Duration
	if form.ExpiresIn != "" {
		d, err := time.ParseDuration(form.ExpiresIn)
		if err != nil || d <= 0 {
			h.renderTokens(w, r, http.StatusBadRequest, `Expiry must be a positive duration with a unit, such as "720h", or empty for a token that never expires.`, form)
			return
		}
		expiresIn = d
	}
	raw, created, err := h.tokens.Create(r.Context(), auth.CreateTokenInput{
		Name:      form.Name,
		Role:      form.Role,
		ExpiresIn: expiresIn,
		By:        uiRequester(principal),
	})
	switch {
	case errors.Is(err, store.ErrTokenExists):
		h.renderTokens(w, r, http.StatusConflict, "A token named "+form.Name+" already exists. Pick another name or revoke the old token first.", form)
		return
	case errors.Is(err, auth.ErrInvalidTokenName), errors.Is(err, core.ErrUnknownRole):
		h.renderTokens(w, r, http.StatusBadRequest, err.Error(), form)
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}
	p := h.newPage(r, "Token created", "tokens", tokenCreatedView{Raw: raw, Token: created})
	h.render(w, r, http.StatusCreated, "token_created", p)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.formPrincipal(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	err := h.tokens.Revoke(r.Context(), name, uiRequester(principal))
	switch {
	case errors.Is(err, core.ErrNotFound):
		h.renderTokens(w, r, http.StatusNotFound, "There is no active token named "+name+".", tokenForm{Role: core.RoleViewer})
	case err != nil:
		h.internalError(w, r, err)
	default:
		seeOther(w, "/tokens", "token_revoked")
	}
}

func uiRequester(p auth.Principal) auth.Requester {
	return auth.Requester{Actor: p.Actor, Origin: map[string]string{"via": "ui"}}
}
