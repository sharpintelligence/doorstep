package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const (
	testGitHubClientID     = "Iv1.test"
	testGitHubClientSecret = "github-client-secret"
)

type fakeGitHub struct {
	srv        *httptest.Server
	mu         sync.Mutex
	challenges map[string]string
	orgCalls   atomic.Int64
	teamCalls  atomic.Int64
	userBody   string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{challenges: map[string]string{}, userBody: `{"id":583231,"login":"OctoCat","name":"The Octocat"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		challenge, ok := f.challenges[r.PostForm.Get("code")]
		delete(f.challenges, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		switch {
		case r.PostForm.Get("client_id") != testGitHubClientID || r.PostForm.Get("client_secret") != testGitHubClientSecret:
			writeTestJSON(w, http.StatusOK, map[string]string{"error": "incorrect_client_credentials"})
		case !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge:
			writeTestJSON(w, http.StatusOK, map[string]string{"error": "bad_verification_code"})
		default:
			w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
			_, _ = w.Write([]byte("access_token=gho_user&scope=read%3Aorg&token_type=bearer"))
		}
	})
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer gho_user" {
				writeTestJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /user", authed(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.userBody))
	}))
	mux.HandleFunc("GET /user/memberships/orgs", authed(func(w http.ResponseWriter, _ *http.Request) {
		f.orgCalls.Add(1)
		_, _ = w.Write([]byte(`[{"organization":{"login":"Example-Org"}}]`))
	}))
	mux.HandleFunc("GET /user/teams", authed(func(w http.ResponseWriter, _ *http.Request) {
		f.teamCalls.Add(1)
		_, _ = w.Write([]byte(`[{"slug":"platform","organization":{"login":"Example-Org"}}]`))
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) authorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	if u.Host != strings.TrimPrefix(f.srv.URL, "http://") || u.Path != "/login/oauth/authorize" {
		t.Fatalf("auth url %s does not point at the fake", authURL)
	}
	if q.Get("client_id") != testGitHubClientID || q.Get("scope") != "read:org" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != testRedirect {
		t.Fatalf("unexpected authorization request %s", authURL)
	}
	code, _ = newSecret()
	f.mu.Lock()
	f.challenges[code] = q.Get("code_challenge")
	f.mu.Unlock()
	return code, q.Get("state")
}

func (f *fakeGitHub) provider(t *testing.T, orgs, teams bool) *GitHubProvider {
	t.Helper()
	p, err := NewGitHubProvider(GitHubOptions{
		APIURL:       f.srv.URL,
		ClientID:     testGitHubClientID,
		ClientSecret: func() (string, error) { return testGitHubClientSecret, nil },
		UserAgent:    "doorstep/test",
		Orgs:         orgs,
		Teams:        teams,
	})
	if err != nil {
		t.Fatalf("NewGitHubProvider: %v", err)
	}
	return p
}

func TestGitHubProviderExchange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		orgs       bool
		teams      bool
		wantGroups []string
	}{
		{name: "users only"},
		{name: "orgs", orgs: true, wantGroups: []string{"Example-Org"}},
		{name: "teams", teams: true, wantGroups: []string{"Example-Org/platform"}},
		{name: "both", orgs: true, teams: true, wantGroups: []string{"Example-Org", "Example-Org/platform"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeGitHub(t)
			p := f.provider(t, tt.orgs, tt.teams)
			verifier := oauth2.GenerateVerifier()
			authURL, err := p.AuthCodeURL(t.Context(), "st", "unused", verifier, testRedirect)
			if err != nil {
				t.Fatalf("AuthCodeURL: %v", err)
			}
			code, state := f.authorize(t, authURL)
			if state != "st" {
				t.Errorf("state = %q", state)
			}
			got, err := p.Exchange(t.Context(), code, "unused", verifier, testRedirect)
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			want := Identity{Subject: "583231", Username: "OctoCat", Name: "The Octocat", Groups: tt.wantGroups}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Exchange = %+v, want %+v", got, want)
			}
			if (f.orgCalls.Load() > 0) != tt.orgs || (f.teamCalls.Load() > 0) != tt.teams {
				t.Errorf("org calls %d, team calls %d", f.orgCalls.Load(), f.teamCalls.Load())
			}
		})
	}
}

func TestGitHubProviderRejects(t *testing.T) {
	t.Parallel()

	f := newFakeGitHub(t)
	p := f.provider(t, false, false)
	verifier := oauth2.GenerateVerifier()
	authURL, err := p.AuthCodeURL(t.Context(), "st", "", verifier, testRedirect)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	code, _ := f.authorize(t, authURL)
	if _, err := p.Exchange(t.Context(), code, "", oauth2.GenerateVerifier(), testRedirect); err == nil || !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("Exchange with wrong verifier = %v", err)
	}

	code, _ = f.authorize(t, authURL)
	p.opts.ClientSecret = func() (string, error) { return "wrong", nil }
	if _, err := p.Exchange(t.Context(), code, "", verifier, testRedirect); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Errorf("Exchange with wrong secret = %v", err)
	}

	f.userBody = `{"id":0}`
	p = f.provider(t, false, false)
	code, _ = f.authorize(t, authURL)
	if _, err := p.Exchange(t.Context(), code, "", verifier, testRedirect); err == nil {
		t.Error("Exchange accepted a user without id")
	}
}

func TestNewGitHubProviderValidates(t *testing.T) {
	t.Parallel()

	secret := func() (string, error) { return "s", nil }
	tests := []struct {
		name string
		opts GitHubOptions
	}{
		{name: "no client id", opts: GitHubOptions{ClientSecret: secret}},
		{name: "no secret", opts: GitHubOptions{ClientID: "x"}},
		{name: "bad api url", opts: GitHubOptions{ClientID: "x", ClientSecret: secret, APIURL: "http://ghe.example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewGitHubProvider(tt.opts); err == nil {
				t.Error("NewGitHubProvider accepted invalid options")
			}
		})
	}
	p, err := NewGitHubProvider(GitHubOptions{ClientID: "x", ClientSecret: secret})
	if err != nil || p.Name() != ProviderGitHub || p.endpoint.AuthURL != "https://github.com/login/oauth/authorize" {
		t.Errorf("default provider = %+v, %v", p, err)
	}
}

func TestGitHubSignInEndToEnd(t *testing.T) {
	t.Parallel()

	f := newFakeGitHub(t)
	mapper := RoleMapper{Provider: ProviderGitHub, Bindings: map[core.Role]Binding{core.RoleDeployer: {Groups: []string{"example-org/platform"}}}}
	orgs, teams := mapper.GitHubLookups()
	e := newEnv(t, testPublicURL, f.provider(t, orgs, teams), mapper)
	fl := &flow{t: t, e: e, names: newCookies(true)}

	rec := fl.do(httptest.NewRequestWithContext(t.Context(), http.MethodGet, LoginPath, http.NoBody))
	code, state := f.authorize(t, rec.Header().Get("Location"))
	rec = fl.callback(url.Values{"state": {state}, "code": {code}}, cookieFrom(t, rec, fl.names.login))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback = %d %q", rec.Code, rec.Body.String())
	}
	got := decodeSeen(t, fl.do(request{session: cookieFrom(t, rec, fl.names.session).Value, path: "/deployer"}.build(t)))
	if !got.Authenticated || got.Actor != "user:github:583231" || got.Role != "deployer" {
		t.Errorf("signed in as %+v", got)
	}
	if f.orgCalls.Load() != 0 || f.teamCalls.Load() != 1 {
		t.Errorf("org calls %d, team calls %d, want only teams", f.orgCalls.Load(), f.teamCalls.Load())
	}
}
