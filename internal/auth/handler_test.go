package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

type flow struct {
	t        *testing.T
	e        *env
	provider *fakeProvider
	names    cookies
}

func newFlow(t *testing.T, publicURL string, provider *fakeProvider, mapper RoleMapper) *flow {
	t.Helper()
	e := newEnv(t, publicURL, provider, mapper)
	return &flow{t: t, e: e, provider: provider, names: newCookies(strings.HasPrefix(publicURL, "https:"))}
}

func defaultFlow(t *testing.T) *flow {
	t.Helper()
	provider := &fakeProvider{identity: Identity{Subject: "sub-1", Username: "alice", Name: "Alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"ops"}}}
	return newFlow(t, testPublicURL, provider, RoleMapper{Provider: ProviderOIDC, Bindings: map[core.Role]Binding{core.RoleDeployer: {Groups: []string{"ops"}}}})
}

func (f *flow) do(req *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	req.Host = "doorstep.example.com"
	return serve(f.t, f.e.handler, req)
}

func (f *flow) start(returnTo string) (state string, loginCookie *http.Cookie) {
	f.t.Helper()
	target := LoginPath
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	rec := f.do(httptest.NewRequestWithContext(f.t.Context(), http.MethodGet, target, http.NoBody))
	if rec.Code != http.StatusFound {
		f.t.Fatalf("GET %s = %d %q", target, rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		f.t.Fatalf("parse Location: %v", err)
	}
	loginCookie = cookieFrom(f.t, rec, f.names.login)
	if loginCookie == nil {
		f.t.Fatal("login did not set the state cookie")
	}
	return location.Query().Get("state"), loginCookie
}

func (f *flow) callback(query url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequestWithContext(f.t.Context(), http.MethodGet, CallbackPath+"?"+query.Encode(), http.NoBody)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return f.do(req)
}

func (f *flow) signIn(returnTo string) (*httptest.ResponseRecorder, *http.Cookie) {
	f.t.Helper()
	state, cookie := f.start(returnTo)
	rec := f.callback(url.Values{"state": {state}, "code": {"the-code"}}, cookie)
	return rec, cookieFrom(f.t, rec, f.names.session)
}

func TestLoginRedirectsToProvider(t *testing.T) {
	t.Parallel()

	f := defaultFlow(t)
	rec := f.do(httptest.NewRequestWithContext(t.Context(), http.MethodGet, LoginPath+"?return_to=%2Fapps%2Fweb", http.NoBody))
	if rec.Code != http.StatusFound || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, headers %v", rec.Code, rec.Header())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || location.Host != "idp.example.com" {
		t.Fatalf("Location = %q", rec.Header().Get("Location"))
	}
	q := location.Query()
	if q.Get("redirect_uri") != testPublicURL+CallbackPath {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	state, nonce, verifier := q.Get("state"), q.Get("nonce"), q.Get("verifier")
	if !wellFormedSecret(state) || !wellFormedSecret(nonce) || len(verifier) < 43 || state == nonce {
		t.Errorf("state %q nonce %q verifier %q", state, nonce, verifier)
	}
	cookie := cookieFrom(t, rec, "__Host-doorstep-login")
	if cookie == nil || cookie.Value != state || cookie.MaxAge != int(loginTTL.Seconds()) {
		t.Fatalf("login cookie = %+v", cookie)
	}
	assertCookieAttributes(t, cookie, true)

	st := f.e.repo.snapshot()
	login, ok := st.logins[hashSecret(state)]
	if !ok || login.ReturnTo != "/apps/web" || login.Nonce != nonce || login.PKCEVerifier != verifier {
		t.Errorf("stored login = %+v, %v", login, ok)
	}
	if st.purged != 1 {
		t.Errorf("expired rows purged %d times, want 1", st.purged)
	}
}

func TestLoginSanitisesReturnPath(t *testing.T) {
	t.Parallel()

	f := defaultFlow(t)
	state, _ := f.start("//evil.example/steal")
	if got := f.e.repo.snapshot().logins[hashSecret(state)].ReturnTo; got != "/" {
		t.Errorf("stored return path = %q, want /", got)
	}
}

func TestLoginProviderUnavailable(t *testing.T) {
	t.Parallel()

	f := defaultFlow(t)
	f.provider.authErr = errors.New("discovery failed: connection refused to idp.internal")
	rec := f.do(httptest.NewRequestWithContext(t.Context(), http.MethodGet, LoginPath, http.NoBody))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "idp.internal") {
		t.Errorf("status = %d body %q", rec.Code, rec.Body.String())
	}
	if len(f.e.repo.snapshot().logins) != 0 || cookieFrom(t, rec, f.names.login) != nil {
		t.Error("a failed login start left state behind")
	}
}

func TestCallbackSignsIn(t *testing.T) {
	t.Parallel()

	f := defaultFlow(t)
	rec, session := f.signIn("/apps/web?tab=history")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/web?tab=history" {
		t.Fatalf("callback = %d Location %q body %q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if session == nil || !wellFormedSecret(session.Value) || session.MaxAge != int(time.Hour.Seconds()) {
		t.Fatalf("session cookie = %+v", session)
	}
	assertCookieAttributes(t, session, true)
	if cleared := cookieFrom(t, rec, f.names.login); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("login cookie not cleared: %+v", cleared)
	}

	ex := f.provider.lastExchange(t)
	if ex.code != "the-code" || ex.redirectURL != testPublicURL+CallbackPath || ex.nonce == "" || ex.verifier == "" {
		t.Errorf("exchange = %+v", ex)
	}

	st := f.e.repo.snapshot()
	if len(st.sessions) != 1 || st.sessions[0].hash != hashSecret(session.Value) {
		t.Fatalf("sessions = %+v", st.sessions)
	}
	s := st.sessions[0]
	if s.Role != core.RoleDeployer || s.Username != "alice" || s.Name != "Alice" || s.Subject != "sub-1" || s.Provider != ProviderOIDC || s.Email != "alice@example.com" {
		t.Errorf("session = %+v", s.Session)
	}
	if len(st.logins) != 0 {
		t.Error("login row not consumed")
	}
	if len(st.audit) != 1 || st.audit[0].Action != core.AuditLogin || st.audit[0].Actor.String() != "user:alice" || st.audit[0].Role != core.RoleDeployer {
		t.Errorf("audit = %+v", st.audit)
	}

	echo := f.do(request{session: session.Value}.build(t))
	if got := decodeSeen(t, echo); !got.Authenticated || got.Actor != "user:alice" || got.Role != "deployer" {
		t.Errorf("signed-in request = %+v", got)
	}
}

func TestCallbackRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		prepare  func(f *flow) (url.Values, *http.Cookie)
		wantCode int
		wantBody string
		exchange bool
	}{
		{
			name: "idp error",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				state, cookie := f.start("")
				return url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {strings.Repeat("x", 1000)}}, cookie
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "cancelled or refused",
		},
		{
			name: "missing state cookie",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				state, _ := f.start("")
				return url.Values{"state": {state}, "code": {"c"}}, nil
			},
			wantCode: http.StatusBadRequest,
			wantBody: "could not be verified",
		},
		{
			name: "state from another browser",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				state, _ := f.start("")
				_, otherCookie := f.start("")
				return url.Values{"state": {state}, "code": {"c"}}, otherCookie
			},
			wantCode: http.StatusBadRequest,
			wantBody: "could not be verified",
		},
		{
			name: "missing state",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				_, cookie := f.start("")
				return url.Values{"code": {"c"}}, cookie
			},
			wantCode: http.StatusBadRequest,
			wantBody: "could not be verified",
		},
		{
			name: "replayed state",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				state, cookie := f.start("")
				query := url.Values{"state": {state}, "code": {"c"}}
				if rec := f.callback(query, cookie); rec.Code != http.StatusSeeOther {
					f.t.Fatalf("first callback = %d", rec.Code)
				}
				return query, cookie
			},
			wantCode: http.StatusBadRequest,
			wantBody: "expired",
			exchange: true,
		},
		{
			name: "expired login",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				state, cookie := f.start("")
				f.e.repo.expireLogins()
				return url.Values{"state": {state}, "code": {"c"}}, cookie
			},
			wantCode: http.StatusBadRequest,
			wantBody: "expired",
		},
		{
			name: "forged state with matching cookie",
			prepare: func(*flow) (url.Values, *http.Cookie) {
				forged, _ := newSecret()
				return url.Values{"state": {forged}, "code": {"c"}}, &http.Cookie{Name: "__Host-doorstep-login", Value: forged}
			},
			wantCode: http.StatusBadRequest,
			wantBody: "expired",
		},
		{
			name: "missing code",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				state, cookie := f.start("")
				return url.Values{"state": {state}}, cookie
			},
			wantCode: http.StatusBadRequest,
			wantBody: "could not be verified",
		},
		{
			name: "exchange fails",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				f.provider.err = errors.New("oidc: id token nonce does not match the login")
				state, cookie := f.start("")
				return url.Values{"state": {state}, "code": {"c"}}, cookie
			},
			wantCode: http.StatusUnauthorized,
			wantBody: "Sign-in failed",
			exchange: true,
		},
		{
			name: "unmapped user",
			prepare: func(f *flow) (url.Values, *http.Cookie) {
				f.provider.identity.Groups = []string{"guests"}
				state, cookie := f.start("")
				return url.Values{"state": {state}, "code": {"c"}}, cookie
			},
			wantCode: http.StatusForbidden,
			wantBody: "not authorised",
			exchange: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := defaultFlow(t)
			query, cookie := tt.prepare(f)
			before := len(f.e.repo.snapshot().sessions)
			rec := f.callback(query, cookie)
			if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("callback = %d %q, want %d containing %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
			if strings.Contains(rec.Body.String(), "nonce") || strings.Contains(rec.Body.String(), "xxxx") {
				t.Errorf("body leaks details: %q", rec.Body.String())
			}
			if cookieFrom(t, rec, f.names.session) != nil {
				t.Error("rejected callback set a session cookie")
			}
			if after := len(f.e.repo.snapshot().sessions); after != before {
				t.Errorf("rejected callback created a session (%d -> %d)", before, after)
			}
			f.provider.mu.Lock()
			exchanged := len(f.provider.exchanges) > 0
			f.provider.mu.Unlock()
			if exchanged != tt.exchange {
				t.Errorf("provider exchange called = %v, want %v", exchanged, tt.exchange)
			}
		})
	}
}

func TestCallbackDefaultRole(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{identity: Identity{Subject: "sub-2", Username: "bob"}}
	f := newFlow(t, testPublicURL, provider, RoleMapper{Provider: ProviderOIDC, Default: core.RoleViewer})
	rec, session := f.signIn("")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" || session == nil {
		t.Fatalf("callback = %d %q", rec.Code, rec.Body.String())
	}
	st := f.e.repo.snapshot()
	if st.sessions[0].Role != core.RoleViewer || st.sessions[0].Name != "bob" {
		t.Errorf("session = %+v", st.sessions[0].Session)
	}
}

func TestCallbackFallsBackToSubjectForUnusableUsername(t *testing.T) {
	t.Parallel()

	provider := &fakeProvider{identity: Identity{Subject: "sub-3", Username: " padded ", Name: ""}}
	f := newFlow(t, testPublicURL, provider, RoleMapper{Provider: ProviderOIDC, Default: core.RoleViewer})
	if rec, _ := f.signIn(""); rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d %q", rec.Code, rec.Body.String())
	}
	if got := f.e.repo.snapshot().audit[0].Actor; got != (core.Actor{Kind: core.ActorUser, ID: "sub-3", Name: "sub-3"}) {
		t.Errorf("actor = %+v", got)
	}
}

func TestCallbackStoreFailure(t *testing.T) {
	t.Parallel()

	f := defaultFlow(t)
	state, cookie := f.start("")
	f.e.repo.fail(errors.New("database down"))
	rec := f.callback(url.Values{"state": {state}, "code": {"c"}}, cookie)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database") {
		t.Errorf("callback = %d %q", rec.Code, rec.Body.String())
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()

	f := defaultFlow(t)
	_, session := f.signIn("")
	logout := func(csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, LogoutPath, strings.NewReader(url.Values{CSRFField: {csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(session)
		return f.do(req)
	}

	if rec := logout("wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without csrf = %d, want 403", rec.Code)
	}
	if len(f.e.repo.snapshot().sessions) != 1 {
		t.Fatal("logout without csrf ended the session")
	}

	rec := logout(csrfTokenFor(session.Value))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("logout = %d %q", rec.Code, rec.Body.String())
	}
	if cleared := cookieFrom(t, rec, f.names.session); cleared == nil || cleared.MaxAge >= 0 {
		t.Errorf("session cookie not cleared: %+v", cleared)
	}
	st := f.e.repo.snapshot()
	if len(st.sessions) != 0 {
		t.Error("session row not deleted")
	}
	if got := f.e.repo.actions(); !slices.Equal(got, []core.AuditAction{core.AuditLogin, core.AuditLogout}) {
		t.Errorf("audit actions = %v", got)
	}
	if last := st.audit[len(st.audit)-1]; last.Actor.String() != "user:alice" || last.Role != core.RoleDeployer {
		t.Errorf("logout audit = %+v", last)
	}

	echo := f.do(request{session: session.Value}.build(t))
	if decodeSeen(t, echo).Authenticated {
		t.Error("session still valid after logout")
	}
}

func TestLogoutWithoutSession(t *testing.T) {
	t.Parallel()

	f := defaultFlow(t)
	rec := f.do(httptest.NewRequestWithContext(t.Context(), http.MethodPost, LogoutPath, http.NoBody))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("anonymous logout = %d", rec.Code)
	}
	if len(f.e.repo.snapshot().audit) != 0 {
		t.Error("anonymous logout wrote an audit row")
	}

	raw := f.e.token(t, core.RoleAdmin)
	rec = f.do(request{method: http.MethodPost, path: LogoutPath, bearer: raw}.build(t))
	if rec.Code != http.StatusSeeOther || len(f.e.repo.actions()) != 1 {
		t.Errorf("token logout = %d, audit %v", rec.Code, f.e.repo.actions())
	}

	rec = f.do(httptest.NewRequestWithContext(t.Context(), http.MethodGet, LogoutPath, http.NoBody))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET %s = %d, want 405", LogoutPath, rec.Code)
	}
}

func TestCookiesFollowPublicURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		publicURL   string
		wantSession string
		wantLogin   string
		wantSecure  bool
	}{
		{publicURL: "https://doorstep.example.com", wantSession: "__Host-doorstep-session", wantLogin: "__Host-doorstep-login", wantSecure: true},
		{publicURL: "https://doorstep.example.com:8443/", wantSession: "__Host-doorstep-session", wantLogin: "__Host-doorstep-login", wantSecure: true},
		{publicURL: "http://127.0.0.1:8080", wantSession: "doorstep-session", wantLogin: "doorstep-login"},
		{publicURL: "http://localhost:8080", wantSession: "doorstep-session", wantLogin: "doorstep-login"},
	}
	for _, tt := range tests {
		t.Run(tt.publicURL, func(t *testing.T) {
			t.Parallel()
			provider := &fakeProvider{identity: Identity{Subject: "s", Username: "u"}}
			f := newFlow(t, tt.publicURL, provider, RoleMapper{Provider: ProviderOIDC, Default: core.RoleViewer})
			state, login := f.start("")
			if login.Name != tt.wantLogin {
				t.Errorf("login cookie name = %q, want %q", login.Name, tt.wantLogin)
			}
			assertCookieAttributes(t, login, tt.wantSecure)
			rec := f.callback(url.Values{"state": {state}, "code": {"c"}}, login)
			session := cookieFrom(t, rec, tt.wantSession)
			if session == nil {
				t.Fatalf("no %s cookie in %v", tt.wantSession, rec.Header())
			}
			assertCookieAttributes(t, session, tt.wantSecure)
			if ex := provider.lastExchange(t); ex.redirectURL != strings.TrimSuffix(tt.publicURL, "/")+CallbackPath {
				t.Errorf("redirect url = %q", ex.redirectURL)
			}
		})
	}
}

func assertCookieAttributes(t *testing.T, c *http.Cookie, secure bool) {
	t.Helper()
	if c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure != secure || c.Domain != "" {
		t.Errorf("cookie %s attributes = path %q httponly %v samesite %v secure %v domain %q", c.Name, c.Path, c.HttpOnly, c.SameSite, c.Secure, c.Domain)
	}
}

func TestNewValidates(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	mapper := RoleMapper{Provider: ProviderOIDC}
	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{name: "no repository", opts: Options{}, wantErr: "repository is required"},
		{name: "tokens only", opts: Options{Repository: repo}},
		{name: "tokens only with public url", opts: Options{Repository: repo, PublicURL: testPublicURL}},
		{name: "provider without public url", opts: Options{Repository: repo, Provider: &fakeProvider{}, Mapper: mapper, SessionTTL: time.Hour}, wantErr: "public url is required"},
		{name: "provider without ttl", opts: Options{Repository: repo, PublicURL: testPublicURL, Provider: &fakeProvider{}, Mapper: mapper}, wantErr: "session ttl"},
		{name: "mapper for another provider", opts: Options{Repository: repo, PublicURL: testPublicURL, Provider: &fakeProvider{}, Mapper: RoleMapper{Provider: ProviderGitHub}, SessionTTL: time.Hour}, wantErr: `role mapping is for "github"`},
		{name: "invalid mapper", opts: Options{Repository: repo, PublicURL: testPublicURL, Provider: &fakeProvider{}, Mapper: RoleMapper{Provider: ProviderOIDC, Default: "root"}, SessionTTL: time.Hour}, wantErr: "role mapping"},
		{name: "public url with path", opts: Options{Repository: repo, PublicURL: "https://example.com/doorstep"}, wantErr: "must not have a path"},
		{name: "public url not absolute", opts: Options{Repository: repo, PublicURL: "example.com"}, wantErr: "absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc, err := New(tt.opts)
			if tt.wantErr == "" {
				if err != nil || svc.Handler() != nil || svc.ProviderName() != "" {
					t.Errorf("New = %v, %v", svc, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("New error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
