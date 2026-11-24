package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const testPublicURL = "https://doorstep.example.com"

type env struct {
	repo    *fakeRepo
	svc     *Service
	handler http.Handler
}

type seen struct {
	Authenticated bool   `json:"authenticated"`
	Actor         string `json:"actor"`
	Role          string `json:"role"`
	Method        string `json:"method"`
	CSRF          string `json:"csrf"`
}

func newEnv(t *testing.T, publicURL string, provider IdentityProvider, mapper RoleMapper) *env {
	t.Helper()
	repo := newFakeRepo()
	svc, err := New(Options{Repository: repo, PublicURL: publicURL, SessionTTL: time.Hour, Provider: provider, Mapper: mapper})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := svc.Authenticator()
	mux := http.NewServeMux()
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		_ = json.NewEncoder(w).Encode(seen{Authenticated: ok, Actor: p.Actor.String(), Role: string(p.Role), Method: string(p.Method), CSRF: CSRFToken(r.Context())})
	})
	mux.Handle("/echo", echo)
	mux.Handle("/viewer", a.RequireRole(core.RoleViewer)(echo))
	mux.Handle("/deployer", a.RequireRole(core.RoleDeployer)(echo))
	mux.Handle("/admin", a.RequireRole(core.RoleAdmin)(echo))
	mux.Handle("/ui", a.RequireRoleOrLogin(core.RoleDeployer)(echo))
	if h := svc.Handler(); h != nil {
		for pattern, route := range h.Routes() {
			mux.Handle(pattern, route)
		}
	}
	return &env{repo: repo, svc: svc, handler: a.Middleware(mux)}
}

func newSessionEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, testPublicURL, &fakeProvider{}, RoleMapper{Provider: ProviderOIDC, Default: core.RoleViewer})
}

func (e *env) session(t *testing.T, role core.Role) string {
	t.Helper()
	raw, hash := newSecret()
	if _, err := e.repo.CreateSession(t.Context(), store.NewSession{Hash: hash, Provider: ProviderOIDC, Subject: "sub", Username: "alice", Name: "Alice", Role: role, TTL: time.Hour}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return raw
}

func (e *env) token(t *testing.T, role core.Role) string {
	t.Helper()
	return mustCreateToken(t, e.svc.Tokens(), "ci", role)
}

func decodeSeen(t *testing.T, rec *httptest.ResponseRecorder) seen {
	t.Helper()
	var s seen
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return s
}

type request struct {
	method   string
	path     string
	bearer   string
	auth     string
	session  string
	csrf     string
	form     url.Values
	body     *bytes.Buffer
	ctype    string
	headers  map[string]string
	hostname string
}

func (r request) build(t *testing.T) *http.Request {
	t.Helper()
	method := r.method
	if method == "" {
		method = http.MethodGet
	}
	path := r.path
	if path == "" {
		path = "/echo"
	}
	var req *http.Request
	switch {
	case r.form != nil:
		req = httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(r.form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case r.body != nil:
		req = httptest.NewRequestWithContext(t.Context(), method, path, r.body)
		req.Header.Set("Content-Type", r.ctype)
	default:
		req = httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
	}
	req.Host = "doorstep.example.com"
	if r.hostname != "" {
		req.Host = r.hostname
	}
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.auth != "" {
		req.Header.Set("Authorization", r.auth)
	}
	if r.session != "" {
		req.AddCookie(&http.Cookie{Name: "__Host-doorstep-session", Value: r.session})
	}
	if r.csrf != "" {
		req.Header.Set(CSRFHeader, r.csrf)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	return req
}

func multipartBody(t *testing.T, field, value string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField(field, value); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func TestMiddleware(t *testing.T) {
	t.Parallel()

	const (
		anonymous = iota
		viaToken
		viaSession
	)
	tests := []struct {
		name       string
		req        func(t *testing.T, e *env) request
		wantCode   int
		wantAs     int
		wantRole   core.Role
		wantClear  bool
		wantErrMsg string
	}{
		{name: "no credentials", req: func(*testing.T, *env) request { return request{} }, wantCode: http.StatusOK},
		{name: "valid token", req: func(t *testing.T, e *env) request {
			t.Helper()
			return request{bearer: e.token(t, core.RoleDeployer)}
		}, wantCode: http.StatusOK, wantAs: viaToken, wantRole: core.RoleDeployer},
		{name: "lowercase bearer scheme", req: func(t *testing.T, e *env) request {
			t.Helper()
			return request{auth: "bearer " + e.token(t, core.RoleViewer)}
		}, wantCode: http.StatusOK, wantAs: viaToken, wantRole: core.RoleViewer},
		{name: "malformed token", req: func(*testing.T, *env) request { return request{bearer: "nope"} }, wantCode: http.StatusUnauthorized, wantErrMsg: "unauthenticated"},
		{name: "empty bearer", req: func(*testing.T, *env) request { return request{auth: "Bearer "} }, wantCode: http.StatusUnauthorized, wantErrMsg: "unauthenticated"},
		{name: "unknown token", req: func(*testing.T, *env) request {
			raw, _ := newAPIToken()
			return request{bearer: raw}
		}, wantCode: http.StatusUnauthorized, wantErrMsg: "unauthenticated"},
		{name: "expired token", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.token(t, core.RoleAdmin)
			e.repo.expireToken("ci")
			return request{bearer: raw}
		}, wantCode: http.StatusUnauthorized, wantErrMsg: "unauthenticated"},
		{name: "revoked token", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.token(t, core.RoleAdmin)
			if err := e.svc.Tokens().Revoke(t.Context(), "ci", cliRoot); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			return request{bearer: raw}
		}, wantCode: http.StatusUnauthorized, wantErrMsg: "unauthenticated"},
		{name: "other authorization scheme is ignored", req: func(*testing.T, *env) request { return request{auth: "Basic YTpi"} }, wantCode: http.StatusOK},
		{name: "session", req: func(t *testing.T, e *env) request {
			t.Helper()
			return request{session: e.session(t, core.RoleAdmin)}
		}, wantCode: http.StatusOK, wantAs: viaSession, wantRole: core.RoleAdmin},
		{name: "expired session", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			e.repo.expireSessions()
			return request{session: raw}
		}, wantCode: http.StatusOK, wantClear: true},
		{name: "unknown session", req: func(*testing.T, *env) request {
			raw, _ := newSecret()
			return request{session: raw}
		}, wantCode: http.StatusOK, wantClear: true},
		{name: "malformed session", req: func(*testing.T, *env) request { return request{session: "garbage"} }, wantCode: http.StatusOK, wantClear: true},
		{name: "bearer wins over session", req: func(t *testing.T, e *env) request {
			t.Helper()
			return request{bearer: e.token(t, core.RoleViewer), session: e.session(t, core.RoleAdmin)}
		}, wantCode: http.StatusOK, wantAs: viaToken, wantRole: core.RoleViewer},
		{name: "bad bearer never falls back to session", req: func(t *testing.T, e *env) request {
			t.Helper()
			return request{bearer: "nope", session: e.session(t, core.RoleAdmin)}
		}, wantCode: http.StatusUnauthorized, wantErrMsg: "unauthenticated"},
		{name: "session post without csrf", req: func(t *testing.T, e *env) request {
			t.Helper()
			return request{method: http.MethodPost, session: e.session(t, core.RoleAdmin)}
		}, wantCode: http.StatusForbidden, wantErrMsg: "forbidden"},
		{name: "session post with csrf header", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, csrf: csrfTokenFor(raw)}
		}, wantCode: http.StatusOK, wantAs: viaSession, wantRole: core.RoleAdmin},
		{name: "session delete with csrf header", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodDelete, session: raw, csrf: csrfTokenFor(raw)}
		}, wantCode: http.StatusOK, wantAs: viaSession, wantRole: core.RoleAdmin},
		{name: "session post with csrf form field", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, form: url.Values{CSRFField: {csrfTokenFor(raw)}, "reason": {"x"}}}
		}, wantCode: http.StatusOK, wantAs: viaSession, wantRole: core.RoleAdmin},
		{name: "session post with csrf multipart field", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			body, ctype := multipartBody(t, CSRFField, csrfTokenFor(raw))
			return request{method: http.MethodPost, session: raw, body: body, ctype: ctype}
		}, wantCode: http.StatusOK, wantAs: viaSession, wantRole: core.RoleAdmin},
		{name: "session post with oversized form", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, form: url.Values{"pad": {strings.Repeat("x", maxFormBytes)}, CSRFField: {csrfTokenFor(raw)}}}
		}, wantCode: http.StatusForbidden, wantErrMsg: "forbidden"},
		{name: "session post with another session's csrf", req: func(t *testing.T, e *env) request {
			t.Helper()
			other, _ := newSecret()
			return request{method: http.MethodPost, session: e.session(t, core.RoleAdmin), csrf: csrfTokenFor(other)}
		}, wantCode: http.StatusForbidden, wantErrMsg: "forbidden"},
		{name: "session post with the session token as csrf", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, csrf: raw}
		}, wantCode: http.StatusForbidden, wantErrMsg: "forbidden"},
		{name: "cross-site fetch metadata", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, csrf: csrfTokenFor(raw), headers: map[string]string{"Sec-Fetch-Site": "cross-site"}}
		}, wantCode: http.StatusForbidden, wantErrMsg: "forbidden"},
		{name: "foreign origin", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, csrf: csrfTokenFor(raw), headers: map[string]string{"Origin": "https://evil.example"}}
		}, wantCode: http.StatusForbidden, wantErrMsg: "forbidden"},
		{name: "public url origin behind a proxy", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, csrf: csrfTokenFor(raw), hostname: "doorstep.internal:8080", headers: map[string]string{"Origin": testPublicURL}}
		}, wantCode: http.StatusOK, wantAs: viaSession, wantRole: core.RoleAdmin},
		{name: "same-origin fetch metadata", req: func(t *testing.T, e *env) request {
			t.Helper()
			raw := e.session(t, core.RoleAdmin)
			return request{method: http.MethodPost, session: raw, csrf: csrfTokenFor(raw), headers: map[string]string{"Sec-Fetch-Site": "same-origin"}}
		}, wantCode: http.StatusOK, wantAs: viaSession, wantRole: core.RoleAdmin},
		{name: "token post needs no csrf", req: func(t *testing.T, e *env) request {
			t.Helper()
			return request{method: http.MethodPost, bearer: e.token(t, core.RoleDeployer), headers: map[string]string{"Sec-Fetch-Site": "cross-site"}}
		}, wantCode: http.StatusOK, wantAs: viaToken, wantRole: core.RoleDeployer},
		{name: "anonymous post passes through", req: func(*testing.T, *env) request { return request{method: http.MethodPost} }, wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newSessionEnv(t)
			rec := serve(t, e.handler, tt.req(t, e).build(t))
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			cleared := cookieFrom(t, rec, "__Host-doorstep-session")
			if tt.wantClear != (cleared != nil && cleared.MaxAge < 0) {
				t.Errorf("session cookie cleared = %v, want %v", cleared, tt.wantClear)
			}
			if tt.wantErrMsg != "" {
				assertJSONError(t, rec, tt.wantCode, tt.wantErrMsg)
				return
			}
			s := decodeSeen(t, rec)
			switch tt.wantAs {
			case anonymous:
				if s.Authenticated {
					t.Errorf("request authenticated as %+v, want anonymous", s)
				}
			case viaToken:
				if !s.Authenticated || s.Method != string(MethodToken) || s.Role != string(tt.wantRole) || s.Actor != "token:ci" || s.CSRF != "" {
					t.Errorf("principal = %+v, want token ci with role %s", s, tt.wantRole)
				}
			case viaSession:
				if !s.Authenticated || s.Method != string(MethodSession) || s.Role != string(tt.wantRole) || s.Actor != "user:oidc:sub" || s.CSRF == "" {
					t.Errorf("principal = %+v, want session alice with role %s", s, tt.wantRole)
				}
			}
		})
	}
}

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	want := `{"error":"` + code + `"}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want exactly %q", rec.Body.String(), want)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", rec.Header())
	}
	hasChallenge := rec.Header().Get("WWW-Authenticate") == `Bearer realm="doorstep"`
	if hasChallenge != (status == http.StatusUnauthorized) {
		t.Errorf("WWW-Authenticate = %q for status %d", rec.Header().Get("WWW-Authenticate"), status)
	}
}

func TestMiddlewareStoreFailure(t *testing.T) {
	t.Parallel()

	for _, viaToken := range []bool{true, false} {
		e := newSessionEnv(t)
		req := request{session: e.session(t, core.RoleViewer)}
		if viaToken {
			req = request{bearer: e.token(t, core.RoleViewer)}
		}
		e.repo.fail(errors.New("database down"))
		rec := serve(t, e.handler, req.build(t))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("token %v: status = %d, want 503", viaToken, rec.Code)
		}
		assertJSONError(t, rec, rec.Code, "unavailable")
	}
}

func TestMiddlewareWithoutSignIn(t *testing.T) {
	t.Parallel()

	e := newEnv(t, "", nil, RoleMapper{})
	if e.svc.Handler() != nil {
		t.Fatal("Handler() is not nil without a provider")
	}
	raw, hash := newSecret()
	if _, err := e.repo.CreateSession(t.Context(), store.NewSession{Hash: hash, Username: "alice", Role: core.RoleAdmin, TTL: time.Hour}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rec := serve(t, e.handler, request{session: raw}.build(t))
	if s := decodeSeen(t, rec); s.Authenticated {
		t.Errorf("session cookie accepted without a sign-in provider: %+v", s)
	}
	rec = serve(t, e.handler, request{bearer: e.token(t, core.RoleAdmin), path: "/admin"}.build(t))
	if rec.Code != http.StatusOK {
		t.Errorf("token request status = %d", rec.Code)
	}
	rec = serve(t, e.handler, request{path: "/ui"}.build(t))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("RequireRoleOrLogin without sign-in = %d, want 401", rec.Code)
	}
	rec = serve(t, e.handler, request{path: LoginPath}.build(t))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET %s without sign-in = %d, want 404", LoginPath, rec.Code)
	}
}

func TestRequireRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path     string
		role     core.Role
		wantCode int
	}{
		{path: "/viewer", wantCode: http.StatusUnauthorized},
		{path: "/viewer", role: core.RoleViewer, wantCode: http.StatusOK},
		{path: "/viewer", role: core.RoleAdmin, wantCode: http.StatusOK},
		{path: "/deployer", wantCode: http.StatusUnauthorized},
		{path: "/deployer", role: core.RoleViewer, wantCode: http.StatusForbidden},
		{path: "/deployer", role: core.RoleDeployer, wantCode: http.StatusOK},
		{path: "/deployer", role: core.RoleAdmin, wantCode: http.StatusOK},
		{path: "/admin", role: core.RoleDeployer, wantCode: http.StatusForbidden},
		{path: "/admin", role: core.RoleAdmin, wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		for _, viaSession := range []bool{false, true} {
			name := tt.path + "/" + string(tt.role)
			if viaSession {
				name += "/session"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				e := newSessionEnv(t)
				req := request{path: tt.path}
				if tt.role != "" && viaSession {
					req.session = e.session(t, tt.role)
				} else if tt.role != "" {
					req.bearer = e.token(t, tt.role)
				}
				rec := serve(t, e.handler, req.build(t))
				if rec.Code != tt.wantCode {
					t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
				}
				switch tt.wantCode {
				case http.StatusUnauthorized:
					assertJSONError(t, rec, rec.Code, "unauthenticated")
				case http.StatusForbidden:
					assertJSONError(t, rec, rec.Code, "forbidden")
				}
			})
		}
	}
}

func TestRequireRoleOrLogin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		method       string
		path         string
		role         core.Role
		wantCode     int
		wantLocation string
	}{
		{name: "anonymous get redirects", method: http.MethodGet, path: "/ui?tab=queue&x=1", wantCode: http.StatusFound, wantLocation: "/auth/login?return_to=%2Fui%3Ftab%3Dqueue%26x%3D1"},
		{name: "anonymous head redirects", method: http.MethodHead, path: "/ui", wantCode: http.StatusFound, wantLocation: "/auth/login?return_to=%2Fui"},
		{name: "anonymous post is 401", method: http.MethodPost, path: "/ui", wantCode: http.StatusUnauthorized},
		{name: "signed in without the role is 403", method: http.MethodGet, path: "/ui", role: core.RoleViewer, wantCode: http.StatusForbidden},
		{name: "signed in with the role", method: http.MethodGet, path: "/ui", role: core.RoleDeployer, wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newSessionEnv(t)
			req := request{method: tt.method, path: tt.path}
			if tt.role != "" {
				req.session = e.session(t, tt.role)
			}
			rec := serve(t, e.handler, req.build(t))
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("Location = %q, want %q", got, tt.wantLocation)
			}
		})
	}
}

func TestRequireRolePanicsOnUnknownRole(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("RequireRole accepted an unknown role")
		}
	}()
	newSessionEnv(t).svc.Authenticator().RequireRole("root")
}

func TestSafeReturnPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: "/"},
		{in: "/", want: "/"},
		{in: "/apps/web?tab=history#latest", want: "/apps/web?tab=history#latest"},
		{in: "/apps/%2Fweb", want: "/apps/%2Fweb"},
		{in: "apps", want: "/"},
		{in: "//evil.example", want: "/"},
		{in: "///evil.example", want: "/"},
		{in: "/\\evil.example", want: "/"},
		{in: "\\\\evil.example", want: "/"},
		{in: "/apps\\..\\x", want: "/"},
		{in: "https://evil.example", want: "/"},
		{in: "https:/evil.example", want: "/"},
		{in: "javascript:alert(1)", want: "/"},
		{in: "/%2F%2Fevil.example", want: "/"},
		{in: "/%2fevil.example", want: "/"},
		{in: "/\t/evil.example", want: "/"},
		{in: "/\r\nLocation: https://evil.example", want: "/"},
		{in: "/%zz", want: "/"},
		{in: "/" + strings.Repeat("a", maxReturnPathLen), want: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := SafeReturnPath(tt.in); got != tt.want {
				t.Errorf("SafeReturnPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
