package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	CSRFHeader       = "X-CSRF-Token"
	CSRFField        = "csrf_token"
	maxFormBytes     = 1 << 20
	maxReturnPathLen = 2048
)

var errCSRF = errors.New("csrf token missing or invalid")

type Authenticator struct {
	repo        Repository
	tokens      *Tokens
	logger      *slog.Logger
	cookies     cookies
	sessions    bool
	crossOrigin *http.CrossOriginProtection
}

func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw, ok := bearerToken(r); ok {
			a.serveBearer(w, r, next, raw)
			return
		}
		if a.sessions {
			if cookie, err := r.Cookie(a.cookies.session); err == nil {
				a.serveSession(w, r, next, cookie.Value)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) serveBearer(w http.ResponseWriter, r *http.Request, next http.Handler, raw string) {
	ctx := r.Context()
	p, err := a.tokens.Authenticate(ctx, raw)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		a.logger.WarnContext(ctx, "authentication failed",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.String("credential", "bearer token"))
		writeUnauthenticated(w)
		return
	case err != nil:
		a.logger.ErrorContext(ctx, "authentication unavailable", slog.String("path", r.URL.Path), slog.Any("error", err))
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, p)))
}

func (a *Authenticator) serveSession(w http.ResponseWriter, r *http.Request, next http.Handler, raw string) {
	ctx := r.Context()
	if !wellFormedSecret(raw) {
		a.cookies.clear(w, a.cookies.session)
		next.ServeHTTP(w, r)
		return
	}
	session, err := a.repo.FindSessionByHash(ctx, hashSecret(raw))
	if errors.Is(err, core.ErrNotFound) {
		a.cookies.clear(w, a.cookies.session)
		next.ServeHTTP(w, r)
		return
	}
	if err != nil {
		a.logger.ErrorContext(ctx, "authentication unavailable", slog.String("path", r.URL.Path), slog.Any("error", err))
		writeJSONError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	p := Principal{
		Actor:  core.Actor{Kind: core.ActorUser, ID: session.Username, Name: session.Name},
		Role:   session.Role,
		Method: MethodSession,
	}
	token := csrfTokenFor(raw)
	if !isSafeMethod(r.Method) {
		if err := a.checkCSRF(w, r, token); err != nil {
			a.logger.WarnContext(ctx, "request rejected",
				slog.String("method", r.Method), slog.String("path", r.URL.Path),
				slog.String("actor", p.Actor.String()), slog.String("reason", err.Error()))
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
	}
	next.ServeHTTP(w, r.WithContext(WithPrincipal(withCSRFToken(ctx, token), p)))
}

func (a *Authenticator) checkCSRF(w http.ResponseWriter, r *http.Request, want string) error {
	if err := a.crossOrigin.Check(r); err != nil {
		return err
	}
	got := r.Header.Get(CSRFHeader)
	if got == "" {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		var err error
		if mediaType == "multipart/form-data" {
			err = r.ParseMultipartForm(maxFormBytes)
		} else {
			err = r.ParseForm()
		}
		if err != nil {
			return fmt.Errorf("%w: %w", errCSRF, err)
		}
		got = r.PostForm.Get(CSRFField)
	}
	if got == "" || !equalSecrets(got, want) {
		return errCSRF
	}
	return nil
}

func (a *Authenticator) RequireRole(role core.Role) func(http.Handler) http.Handler {
	return a.require(role, false)
}

func (a *Authenticator) RequireRoleOrLogin(role core.Role) func(http.Handler) http.Handler {
	return a.require(role, a.sessions)
}

func (a *Authenticator) require(role core.Role, redirect bool) func(http.Handler) http.Handler {
	if !role.Valid() {
		panic(fmt.Sprintf("auth: unknown role %q", role))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			p, ok := PrincipalFrom(ctx)
			switch {
			case !ok && redirect && (r.Method == http.MethodGet || r.Method == http.MethodHead):
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, LoginPath+"?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			case !ok:
				a.logger.WarnContext(ctx, "authentication required",
					slog.String("method", r.Method), slog.String("path", r.URL.Path))
				writeUnauthenticated(w)
			case !p.Allows(role):
				a.logger.WarnContext(ctx, "authorization denied",
					slog.String("method", r.Method), slog.String("path", r.URL.Path),
					slog.String("actor", p.Actor.String()), slog.String("role", string(p.Role)), slog.String("required", string(role)))
				writeJSONError(w, http.StatusForbidden, "forbidden")
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func SafeReturnPath(raw string) string {
	switch {
	case raw == "" || len(raw) > maxReturnPathLen || raw[0] != '/':
		return "/"
	case strings.HasPrefix(raw, "//"), strings.ContainsRune(raw, '\\'), strings.ContainsFunc(raw, unicode.IsControl):
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return raw
}

func writeUnauthenticated(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="doorstep"`)
	writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
