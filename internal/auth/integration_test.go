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
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func TestSignInAgainstPostgres(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	repo := NewRepository(st)
	provider := &fakeProvider{identity: Identity{Subject: "sub-1", Username: "alice", Name: "Alice", Groups: []string{"ops"}}}
	svc, err := New(Options{
		Repository: repo,
		PublicURL:  testPublicURL,
		SessionTTL: time.Hour,
		Provider:   provider,
		Mapper:     RoleMapper{Provider: ProviderOIDC, Bindings: map[core.Role]Binding{core.RoleDeployer: {Groups: []string{"ops"}}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	for pattern, h := range svc.Handler().Routes() {
		mux.Handle(pattern, h)
	}
	mux.Handle("POST /deploy", svc.Authenticator().RequireRole(core.RoleDeployer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		_, _ = w.Write([]byte(p.Actor.String()))
	})))
	handler := svc.Authenticator().Middleware(mux)
	do := func(req *http.Request) *httptest.ResponseRecorder {
		req.Host = "doorstep.example.com"
		return serve(t, handler, req)
	}
	names := newCookies(true)

	rec := do(httptest.NewRequestWithContext(t.Context(), http.MethodGet, LoginPath+"?return_to=%2Fqueue", http.NoBody))
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d %q", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	state := location.Query().Get("state")
	callback := httptest.NewRequestWithContext(t.Context(), http.MethodGet, CallbackPath+"?"+url.Values{"state": {state}, "code": {"c"}}.Encode(), http.NoBody)
	callback.AddCookie(cookieFrom(t, rec, names.login))
	rec = do(callback)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/queue" {
		t.Fatalf("callback = %d %q", rec.Code, rec.Body.String())
	}
	session := cookieFrom(t, rec, names.session)

	if stored, err := st.FindSessionByHash(t.Context(), hashSecret(session.Value)); err != nil || stored.Role != core.RoleDeployer || stored.Username != "alice" {
		t.Fatalf("stored session = %+v, %v", stored, err)
	}
	if _, err := st.ConsumeLogin(t.Context(), hashSecret(state)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("login row was not consumed: %v", err)
	}

	post := func(path, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, http.NoBody)
		req.AddCookie(session)
		if csrf != "" {
			req.Header.Set(CSRFHeader, csrf)
		}
		return do(req)
	}
	if rec := post("/deploy", ""); rec.Code != http.StatusForbidden {
		t.Errorf("deploy without csrf = %d", rec.Code)
	}
	if rec := post("/deploy", csrfTokenFor(session.Value)); rec.Code != http.StatusOK || rec.Body.String() != "user:oidc:sub-1" {
		t.Errorf("deploy = %d %q", rec.Code, rec.Body.String())
	}
	if rec := post(LogoutPath, csrfTokenFor(session.Value)); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", rec.Code)
	}
	if rec := post("/deploy", csrfTokenFor(session.Value)); rec.Code != http.StatusUnauthorized {
		t.Errorf("deploy after logout = %d, want 401", rec.Code)
	}

	raw, _, err := svc.Tokens().Create(t.Context(), CreateTokenInput{Name: "ci", Role: core.RoleViewer, By: cliRoot})
	if err != nil {
		t.Fatalf("Create token: %v", err)
	}
	if p, err := svc.Tokens().Authenticate(t.Context(), raw); err != nil || p.Role != core.RoleViewer {
		t.Errorf("Authenticate = %+v, %v", p, err)
	}
	if err := svc.Tokens().Revoke(t.Context(), "ci", cliRoot); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := svc.Tokens().Authenticate(t.Context(), raw); err == nil {
		t.Error("revoked token still authenticates")
	}

	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var actions []string
	for _, e := range slices.Backward(entries) {
		actions = append(actions, string(e.Action)+" "+e.Actor.String())
	}
	want := []string{"auth.login user:oidc:sub-1", "auth.logout user:oidc:sub-1", "token.create cli:root", "token.revoke cli:root"}
	if !slices.Equal(actions, want) {
		t.Errorf("audit = %v, want %v", actions, want)
	}
	for _, e := range entries {
		if strings.Contains(string(e.Detail), raw) || strings.Contains(string(e.Detail), session.Value) {
			t.Errorf("audit detail leaks a secret: %s", e.Detail)
		}
	}
}
