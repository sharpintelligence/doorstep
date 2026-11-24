package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const (
	loginTTL          = 10 * time.Minute
	maxLoggedIdPError = 200
)

type Handler struct {
	repo        Repository
	provider    IdentityProvider
	mapper      RoleMapper
	cookies     cookies
	redirectURL string
	sessionTTL  time.Duration
	logger      *slog.Logger
}

func (h *Handler) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"GET " + LoginPath:    http.HandlerFunc(h.login),
		"GET " + CallbackPath: http.HandlerFunc(h.callback),
		"POST " + LogoutPath:  http.HandlerFunc(h.logout),
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	returnTo := SafeReturnPath(r.URL.Query().Get("return_to"))
	if _, err := h.repo.DeleteExpiredAuthRows(ctx); err != nil {
		h.logger.WarnContext(ctx, "purging expired sessions failed", slog.Any("error", err))
	}

	state, stateHash := newSecret()
	nonce, _ := newSecret()
	verifier := oauth2.GenerateVerifier()
	target, err := h.provider.AuthCodeURL(ctx, state, nonce, verifier, h.redirectURL)
	if err != nil {
		h.logger.ErrorContext(ctx, "sign-in unavailable", slog.String("provider", h.provider.Name()), slog.Any("error", err))
		http.Error(w, "Sign-in is unavailable. Try again later.", http.StatusServiceUnavailable)
		return
	}
	err = h.repo.CreateLogin(ctx, store.NewLogin{
		StateHash:    stateHash,
		Nonce:        nonce,
		PKCEVerifier: verifier,
		ReturnTo:     returnTo,
		TTL:          loginTTL,
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "starting sign-in failed", slog.Any("error", err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h.cookies.set(w, h.cookies.login, state, loginTTL)
	http.Redirect(w, r, target, http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	h.cookies.clear(w, h.cookies.login)
	query := r.URL.Query()

	if idpError := query.Get("error"); idpError != "" {
		h.logger.WarnContext(ctx, "sign-in refused by identity provider",
			slog.String("error", truncate(idpError)), slog.String("description", truncate(query.Get("error_description"))))
		http.Error(w, "Sign-in was cancelled or refused by the identity provider.", http.StatusUnauthorized)
		return
	}
	state := query.Get("state")
	cookie, err := r.Cookie(h.cookies.login)
	if state == "" || err != nil || !equalSecrets(cookie.Value, state) {
		h.logger.WarnContext(ctx, "sign-in rejected", slog.String("reason", "state does not match the browser"))
		http.Error(w, "Sign-in could not be verified. Start again from the sign-in page.", http.StatusBadRequest)
		return
	}
	login, err := h.repo.ConsumeLogin(ctx, hashSecret(state))
	if errors.Is(err, core.ErrNotFound) {
		h.logger.WarnContext(ctx, "sign-in rejected", slog.String("reason", "unknown, used or expired state"))
		http.Error(w, "Sign-in expired. Start again from the sign-in page.", http.StatusBadRequest)
		return
	}
	if err != nil {
		h.logger.ErrorContext(ctx, "completing sign-in failed", slog.Any("error", err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	code := query.Get("code")
	if code == "" {
		h.logger.WarnContext(ctx, "sign-in rejected", slog.String("reason", "no authorization code"))
		http.Error(w, "Sign-in could not be verified. Start again from the sign-in page.", http.StatusBadRequest)
		return
	}

	identity, err := h.provider.Exchange(ctx, code, login.Nonce, login.PKCEVerifier, h.redirectURL)
	if err != nil {
		h.logger.WarnContext(ctx, "sign-in failed", slog.String("provider", h.provider.Name()), slog.Any("error", err))
		http.Error(w, "Sign-in failed. Start again from the sign-in page.", http.StatusUnauthorized)
		return
	}
	actor := identityActor(identity)
	role, ok := h.mapper.Resolve(identity)
	if !ok {
		h.logger.WarnContext(ctx, "sign-in denied", slog.String("actor", actor.String()), slog.String("subject", identity.Subject))
		http.Error(w, "Your account is not authorised to use doorstep. Ask an administrator for access.", http.StatusForbidden)
		return
	}

	raw, hash := newSecret()
	err = h.repo.InTx(ctx, func(tx Repository) error {
		if _, err := tx.CreateSession(ctx, store.NewSession{
			Hash:     hash,
			Provider: h.provider.Name(),
			Subject:  identity.Subject,
			Username: actor.ID,
			Name:     actor.Name,
			Email:    identity.Email,
			Role:     role,
			TTL:      h.sessionTTL,
		}); err != nil {
			return err
		}
		return tx.RecordAudit(ctx, core.AuditEntry{
			Actor:  actor,
			Role:   role,
			Action: core.AuditLogin,
			Detail: mustJSON(map[string]any{"provider": h.provider.Name(), "subject": identity.Subject}),
		})
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "creating session failed", slog.Any("error", err))
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h.cookies.set(w, h.cookies.session, raw, h.sessionTTL)
	h.logger.InfoContext(ctx, "signed in", slog.String("actor", actor.String()), slog.String("role", string(role)))
	http.Redirect(w, r, login.ReturnTo, http.StatusSeeOther)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	p, ok := PrincipalFrom(ctx)
	cookie, err := r.Cookie(h.cookies.session)
	if ok && p.Method == MethodSession && err == nil {
		err := h.repo.InTx(ctx, func(tx Repository) error {
			if _, err := tx.DeleteSessionByHash(ctx, hashSecret(cookie.Value)); err != nil {
				return err
			}
			return tx.RecordAudit(ctx, core.AuditEntry{Actor: p.Actor, Role: p.Role, Action: core.AuditLogout})
		})
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			h.logger.ErrorContext(ctx, "ending session failed", slog.Any("error", err))
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		h.logger.InfoContext(ctx, "signed out", slog.String("actor", p.Actor.String()))
	}
	h.cookies.clear(w, h.cookies.session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func identityActor(id Identity) core.Actor {
	actor := core.Actor{Kind: core.ActorUser, ID: id.Username, Name: id.Name}
	if actor.Validate() != nil {
		actor.ID = id.Subject
	}
	if actor.Name == "" {
		actor.Name = actor.ID
	}
	return actor
}

func truncate(s string) string {
	if len(s) <= maxLoggedIdPError {
		return s
	}
	return strings.ToValidUTF8(s[:maxLoggedIdPError], "") + "…"
}
