package web

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const publicURL = "https://doorstep.example"

type memorySession struct {
	store.Session
	hash [32]byte
}

type memoryAuth struct {
	auth.Repository
	mu       sync.Mutex
	logins   map[[32]byte]store.Login
	sessions []memorySession
	audit    []core.AuditEntry
}

func newMemoryAuth() *memoryAuth {
	return &memoryAuth{logins: map[[32]byte]store.Login{}}
}

func (m *memoryAuth) InTx(_ context.Context, fn func(auth.Repository) error) error {
	return fn(m)
}

func (m *memoryAuth) CreateLogin(_ context.Context, l store.NewLogin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logins[l.StateHash] = store.Login{Nonce: l.Nonce, PKCEVerifier: l.PKCEVerifier, ReturnTo: l.ReturnTo}
	return nil
}

func (m *memoryAuth) ConsumeLogin(_ context.Context, stateHash [32]byte) (store.Login, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logins[stateHash]
	if !ok {
		return store.Login{}, core.ErrNotFound
	}
	delete(m.logins, stateHash)
	return l, nil
}

func (m *memoryAuth) DeleteExpiredAuthRows(context.Context) (int64, error) {
	return 0, nil
}

func (m *memoryAuth) CreateSession(_ context.Context, n store.NewSession) (store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := store.Session{
		ID: int64(len(m.sessions) + 1), Provider: n.Provider, Subject: n.Subject, Username: n.Username, Name: n.Name, Role: n.Role,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(n.TTL),
	}
	m.sessions = append(m.sessions, memorySession{Session: s, hash: n.Hash})
	return s, nil
}

func (m *memoryAuth) FindSessionByHash(_ context.Context, hash [32]byte) (store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.hash == hash {
			return s.Session, nil
		}
	}
	return store.Session{}, core.ErrNotFound
}

func (m *memoryAuth) DeleteSessionByHash(_ context.Context, hash [32]byte) (store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.sessions {
		if s.hash == hash {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			return s.Session, nil
		}
	}
	return store.Session{}, core.ErrNotFound
}

func (m *memoryAuth) RecordAudit(_ context.Context, e core.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	return nil
}

type fakeIdP struct {
	mu       sync.Mutex
	subject  string
	username string
}

func (p *fakeIdP) Name() string { return auth.ProviderOIDC }

func (p *fakeIdP) AuthCodeURL(_ context.Context, state, _, _, _ string) (string, error) {
	return "https://idp.example/authorize?state=" + url.QueryEscape(state), nil
}

func (p *fakeIdP) Exchange(context.Context, string, string, string, string) (auth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return auth.Identity{Subject: p.subject, Username: p.username}, nil
}

type browser struct {
	t       *testing.T
	handler http.Handler
	cookies map[string]*http.Cookie
}

func (b *browser) do(method, target string, form url.Values, header map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	var body io.Reader = http.NoBody
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequestWithContext(b.t.Context(), method, publicURL+target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	for _, c := range b.cookies {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, r)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
			continue
		}
		b.cookies[c.Name] = c
	}
	return rec
}

func (b *browser) signIn(returnTo string) *httptest.ResponseRecorder {
	b.t.Helper()
	login := b.do(http.MethodGet, auth.LoginPath+"?return_to="+url.QueryEscape(returnTo), nil, nil)
	if login.Code != http.StatusFound {
		b.t.Fatalf("login = %d %s", login.Code, login.Body.String())
	}
	idp, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		b.t.Fatalf("parse IdP redirect: %v", err)
	}
	return b.do(http.MethodGet, auth.CallbackPath+"?code=c&state="+url.QueryEscape(idp.Query().Get("state")), nil, nil)
}

var csrfPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func (b *browser) csrfToken(path string) string {
	b.t.Helper()
	rec := b.do(http.MethodGet, path, nil, nil)
	m := csrfPattern.FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		b.t.Fatalf("GET %s = %d without a csrf token:\n%s", path, rec.Code, rec.Body.String())
	}
	return m[1]
}

type realAuth struct {
	fakes *fakes
	idp   *fakeIdP
	mux   http.Handler
}

func newRealAuth(t *testing.T) *realAuth {
	t.Helper()
	idp := &fakeIdP{subject: "alice-sub", username: "alice"}
	service, err := auth.New(auth.Options{
		Repository: newMemoryAuth(),
		PublicURL:  publicURL,
		SessionTTL: time.Hour,
		Provider:   idp,
		Mapper: auth.RoleMapper{Provider: auth.ProviderOIDC, Bindings: map[core.Role]auth.Binding{
			core.RoleDeployer: {Users: []string{"alice-sub"}},
			core.RoleViewer:   {Users: []string{"victor-sub"}},
		}},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	authenticator := service.Authenticator()
	f := richFakes()
	f.locks.locks = nil
	opts := f.options(io.Discard)
	opts.RequireLogin = authenticator.RequireRoleOrLogin(core.RoleViewer)
	opts.SignIn = true
	h, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	routes := h.Routes()
	maps.Copy(routes, service.Handler().Routes())
	mux := http.NewServeMux()
	for pattern, handler := range routes {
		mux.Handle(pattern, authenticator.Middleware(handler))
	}
	return &realAuth{fakes: f, idp: idp, mux: mux}
}

func (a *realAuth) browser(t *testing.T) *browser {
	t.Helper()
	return &browser{t: t, handler: a.mux, cookies: map[string]*http.Cookie{}}
}

func TestRealAuthenticator(t *testing.T) {
	t.Parallel()

	t.Run("anonymous page redirects to sign-in and comes back", func(t *testing.T) {
		t.Parallel()
		a := newRealAuth(t)
		b := a.browser(t)
		rec := b.do(http.MethodGet, "/apps/blog?before=7", nil, nil)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/login?return_to=%2Fapps%2Fblog%3Fbefore%3D7" {
			t.Fatalf("anonymous GET = %d %q", rec.Code, rec.Header().Get("Location"))
		}
		callback := b.signIn("/apps/blog?before=7")
		if callback.Code != http.StatusSeeOther || callback.Header().Get("Location") != "/apps/blog?before=7" {
			t.Fatalf("callback = %d %q", callback.Code, callback.Header().Get("Location"))
		}
		if page := b.do(http.MethodGet, "/apps/blog?before=7", nil, nil); page.Code != http.StatusOK {
			t.Fatalf("signed-in GET = %d", page.Code)
		}
	})

	t.Run("anonymous htmx poll gets a redirect header", func(t *testing.T) {
		t.Parallel()
		b := newRealAuth(t).browser(t)
		rec := b.do(http.MethodGet, "/locks", nil, map[string]string{"HX-Request": "true", "HX-Current-URL": publicURL + "/locks"})
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/auth/login?return_to=%2Flocks" {
			t.Fatalf("htmx GET = %d HX-Redirect %q", rec.Code, rec.Header().Get("HX-Redirect"))
		}
	})

	t.Run("csrf", func(t *testing.T) {
		t.Parallel()
		a := newRealAuth(t)
		b := a.browser(t)
		b.signIn("/")
		token := b.csrfToken("/apps/blog")
		page := b.do(http.MethodGet, "/apps/blog", nil, nil).Body.String()
		if !strings.Contains(page, `hx-headers="{&#34;X-CSRF-Token&#34;:&#34;`+token+`&#34;}"`) {
			t.Errorf("body hx-headers does not carry the token")
		}

		steps := []struct {
			name       string
			form       url.Values
			header     map[string]string
			wantStatus int
		}{
			{name: "missing token", form: url.Values{"reason": {"a"}}, wantStatus: http.StatusForbidden},
			{name: "wrong token", form: url.Values{"reason": {"a"}, "csrf_token": {"nope"}}, wantStatus: http.StatusForbidden},
			{name: "cross-origin", form: url.Values{"reason": {"a"}, "csrf_token": {token}}, header: map[string]string{"Sec-Fetch-Site": "cross-site"}, wantStatus: http.StatusForbidden},
			{name: "other origin", form: url.Values{"reason": {"a"}, "csrf_token": {token}}, header: map[string]string{"Origin": "https://evil.example"}, wantStatus: http.StatusForbidden},
			{name: "form token", form: url.Values{"reason": {"a"}, "csrf_token": {token}}, header: map[string]string{"Origin": publicURL}, wantStatus: http.StatusSeeOther},
			{name: "header token", form: url.Values{"reason": {"b"}}, header: map[string]string{"X-CSRF-Token": token, "Sec-Fetch-Site": "same-origin"}, wantStatus: http.StatusSeeOther},
		}
		for _, step := range steps {
			rec := b.do(http.MethodPost, "/apps/blog/lock", step.form, step.header)
			if rec.Code != step.wantStatus {
				t.Fatalf("%s: POST = %d, want %d: %s", step.name, rec.Code, step.wantStatus, rec.Body.String())
			}
		}
		if got := len(a.fakes.locks.requests); got != 2 {
			t.Fatalf("lock requests = %d, want 2 (only the requests with a valid token)", got)
		}
		if req := a.fakes.locks.requests[0]; req.Actor.String() != "user:oidc:alice-sub" {
			t.Errorf("lock actor = %q", req.Actor)
		}
	})

	t.Run("csrf on the block form", func(t *testing.T) {
		t.Parallel()
		a := newRealAuth(t)
		b := a.browser(t)
		b.signIn("/")
		token := b.csrfToken("/apps/blog")
		form := func(token string) url.Values {
			return url.Values{"ref": {shaApp}, "reason": {"bad"}, "csrf_token": {token}}
		}
		steps := []struct {
			name       string
			form       url.Values
			header     map[string]string
			wantStatus int
		}{
			{name: "missing token", form: url.Values{"ref": {shaApp}, "reason": {"bad"}}, wantStatus: http.StatusForbidden},
			{name: "wrong token", form: form("nope"), wantStatus: http.StatusForbidden},
			{name: "cross-origin", form: form(token), header: map[string]string{"Sec-Fetch-Site": "cross-site"}, wantStatus: http.StatusForbidden},
			{name: "valid", form: form(token), header: map[string]string{"Origin": publicURL}, wantStatus: http.StatusSeeOther},
		}
		for _, step := range steps {
			if rec := b.do(http.MethodPost, "/apps/blog/block", step.form, step.header); rec.Code != step.wantStatus {
				t.Fatalf("%s: POST = %d, want %d: %s", step.name, rec.Code, step.wantStatus, rec.Body.String())
			}
		}
		if got := len(a.fakes.rollbacks.blockReqs); got != 1 {
			t.Fatalf("block requests = %d, want 1", got)
		}
	})

	t.Run("viewer session is refused with an HTML page", func(t *testing.T) {
		t.Parallel()
		a := newRealAuth(t)
		a.idp.subject, a.idp.username = "victor-sub", "victor"
		b := a.browser(t)
		b.signIn("/")
		token := b.csrfToken("/apps/blog")
		rec := b.do(http.MethodPost, "/apps/blog/lock", url.Values{"reason": {"a"}, "csrf_token": {token}}, nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "needs the deployer role") {
			t.Fatalf("viewer POST = %d %s: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		if len(a.fakes.locks.requests) != 0 {
			t.Fatalf("viewer locked an app")
		}
		if page := b.do(http.MethodGet, "/apps/blog", nil, nil).Body.String(); strings.Contains(page, `action="/apps/blog/lock"`) {
			t.Errorf("viewer sees the lock form")
		}
	})

	t.Run("sign out ends the session", func(t *testing.T) {
		t.Parallel()
		b := newRealAuth(t).browser(t)
		b.signIn("/")
		token := b.csrfToken("/")
		if rec := b.do(http.MethodPost, auth.LogoutPath, url.Values{"csrf_token": {token}}, nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("logout = %d", rec.Code)
		}
		if rec := b.do(http.MethodGet, "/", nil, nil); rec.Code != http.StatusFound {
			t.Fatalf("GET after logout = %d, want a redirect to sign-in", rec.Code)
		}
	})
}
