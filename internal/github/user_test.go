package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const userToken = "gho_usertoken"

type fakeUserAPI struct {
	t       *testing.T
	srv     *httptest.Server
	orgs    []string
	teams   []string
	calls   atomic.Int64
	user    string
	linkFor func(path string, page int) string
}

func newFakeUserAPI(t *testing.T) *fakeUserAPI {
	t.Helper()
	f := &fakeUserAPI{t: t, user: `{"id":583231,"login":"octocat","name":"The Octocat"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(f.user))
	})
	mux.HandleFunc("GET /user/memberships/orgs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "active" {
			http.Error(w, "state must be active", http.StatusBadRequest)
			return
		}
		f.servePage(w, r, f.orgs, func(org string) string {
			return fmt.Sprintf(`{"state":"active","organization":{"login":%q}}`, org)
		})
	})
	mux.HandleFunc("GET /user/teams", func(w http.ResponseWriter, r *http.Request) {
		f.servePage(w, r, f.teams, func(team string) string {
			org, slug, _ := strings.Cut(team, "/")
			return fmt.Sprintf(`{"slug":%q,"organization":{"login":%q}}`, slug, org)
		})
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		switch {
		case r.Header.Get("Authorization") != "Bearer "+userToken:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
		case r.Header.Get("X-GitHub-Api-Version") != apiVersion || r.Header.Get("User-Agent") != "doorstep/test":
			http.Error(w, "missing headers", http.StatusBadRequest)
		default:
			mux.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUserAPI) servePage(w http.ResponseWriter, r *http.Request, items []string, render func(string) string) {
	perPage, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || perPage != userPageSize {
		http.Error(w, "per_page must be 100", http.StatusBadRequest)
		return
	}
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	if end < len(items) {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(page+1))
		next := f.srv.URL + r.URL.Path + "?" + query.Encode()
		if f.linkFor != nil {
			next = f.linkFor(r.URL.Path, page+1)
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, f.srv.URL+r.URL.Path+"?page=999"))
	}
	parts := make([]string, 0, end-start)
	for _, item := range items[start:end] {
		parts = append(parts, render(item))
	}
	_, _ = w.Write([]byte("[" + strings.Join(parts, ",") + "]"))
}

func (f *fakeUserAPI) client(t *testing.T) *UserClient {
	t.Helper()
	c, err := NewUserClient(UserOptions{BaseURL: f.srv.URL, Token: userToken, UserAgent: "doorstep/test"})
	if err != nil {
		t.Fatalf("NewUserClient: %v", err)
	}
	return c
}

func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

func TestUserClientUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    User
		wantErr string
	}{
		{name: "valid", body: `{"id":583231,"login":"octocat","name":"The Octocat"}`, want: User{ID: 583231, Login: "octocat", Name: "The Octocat"}},
		{name: "no name", body: `{"id":1,"login":"ghost","name":null}`, want: User{ID: 1, Login: "ghost"}},
		{name: "missing login", body: `{"id":1}`, wantErr: "no id or login"},
		{name: "missing id", body: `{"login":"ghost"}`, wantErr: "no id or login"},
		{name: "malformed", body: `{`, wantErr: "decode GET /user response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := newFakeUserAPI(t)
			api.user = tt.body
			got, err := api.client(t).User(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("User() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("User() = %+v, %v, want %+v", got, err, tt.want)
			}
		})
	}
}

func TestUserClientMembershipsPaginate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		orgs      int
		teams     int
		wantOrgs  int
		wantTeams int
		wantCalls int64
	}{
		{name: "none", wantCalls: 2},
		{name: "one page", orgs: 3, teams: 100, wantOrgs: 3, wantTeams: 100, wantCalls: 2},
		{name: "several pages", orgs: 250, teams: 101, wantOrgs: 250, wantTeams: 101, wantCalls: 5},
		{name: "capped", orgs: 1100, teams: 0, wantOrgs: MaxUserPages * userPageSize, wantCalls: MaxUserPages + 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api := newFakeUserAPI(t)
			api.orgs = names("org", tt.orgs)
			teams := names("org0/team", tt.teams)
			api.teams = teams
			c := api.client(t)

			orgs, err := c.OrgLogins(t.Context())
			if err != nil {
				t.Fatalf("OrgLogins: %v", err)
			}
			if len(orgs) != tt.wantOrgs || !slices.Equal(orgs, api.orgs[:tt.wantOrgs]) {
				t.Errorf("OrgLogins returned %d orgs, want %d", len(orgs), tt.wantOrgs)
			}
			slugs, err := c.TeamSlugs(t.Context())
			if err != nil {
				t.Fatalf("TeamSlugs: %v", err)
			}
			if len(slugs) != tt.wantTeams || !slices.Equal(slugs, teams[:tt.wantTeams]) {
				t.Errorf("TeamSlugs returned %d teams, want %d", len(slugs), tt.wantTeams)
			}
			if got := api.calls.Load(); got != tt.wantCalls {
				t.Errorf("made %d requests, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestUserClientRefusesForeignNextLink(t *testing.T) {
	t.Parallel()

	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached the foreign host")
	}))
	t.Cleanup(foreign.Close)

	api := newFakeUserAPI(t)
	api.orgs = names("org", 150)
	api.linkFor = func(path string, page int) string { return fmt.Sprintf("%s%s?page=%d", foreign.URL, path, page) }
	_, err := api.client(t).OrgLogins(t.Context())
	if !errors.Is(err, ErrForeignHost) {
		t.Errorf("OrgLogins error = %v, want ErrForeignHost", err)
	}
}

func TestUserClientErrors(t *testing.T) {
	t.Parallel()

	api := newFakeUserAPI(t)
	c, err := NewUserClient(UserOptions{BaseURL: api.srv.URL, Token: "wrong", UserAgent: "doorstep/test"})
	if err != nil {
		t.Fatalf("NewUserClient: %v", err)
	}
	_, err = c.User(t.Context())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || apiErr.Message != "Bad credentials" {
		t.Errorf("User() error = %v, want 401 Bad credentials", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("error leaks the token: %v", err)
	}

	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintf(w, `{"message":%q}`, strings.Repeat("x", 4096))
	}))
	t.Cleanup(long.Close)
	c, err = NewUserClient(UserOptions{BaseURL: long.URL, Token: userToken})
	if err != nil {
		t.Fatalf("NewUserClient: %v", err)
	}
	_, err = c.TeamSlugs(t.Context())
	if !errors.As(err, &apiErr) || len(apiErr.Message) > maxErrorMessageBytes+len("…") {
		t.Errorf("TeamSlugs error = %v, want a truncated 403", err)
	}
}

func TestNewUserClientValidates(t *testing.T) {
	t.Parallel()

	if _, err := NewUserClient(UserOptions{Token: ""}); err == nil {
		t.Error("NewUserClient accepted an empty token")
	}
	if _, err := NewUserClient(UserOptions{BaseURL: "http://api.example.com", Token: userToken}); !errors.Is(err, ErrInvalidBaseURL) {
		t.Errorf("plain http base URL error = %v, want ErrInvalidBaseURL", err)
	}
	if _, err := NewUserClient(UserOptions{Token: userToken}); err != nil {
		t.Errorf("default base URL: %v", err)
	}
}

func TestNextLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header []string
		want   string
	}{
		{name: "none"},
		{name: "next and last", header: []string{`<https://api.github.com/user/teams?page=2>; rel="next", <https://api.github.com/user/teams?page=5>; rel="last"`}, want: "https://api.github.com/user/teams?page=2"},
		{name: "last only", header: []string{`<https://api.github.com/user/teams?page=5>; rel="last"`}},
		{name: "next second", header: []string{`<https://a/1>; rel="prev", <https://a/3>; rel="next"`}, want: "https://a/3"},
		{name: "separate headers", header: []string{`<https://a/1>; rel="prev"`, `<https://a/3>; rel="next"`}, want: "https://a/3"},
		{name: "malformed", header: []string{`https://a/3; rel="next"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			for _, v := range tt.header {
				h.Add("Link", v)
			}
			if got := nextLink(h); got != tt.want {
				t.Errorf("nextLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOAuthEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		apiURL    string
		wantAuth  string
		wantToken string
		wantErr   bool
	}{
		{apiURL: "", wantAuth: "https://github.com/login/oauth/authorize", wantToken: "https://github.com/login/oauth/access_token"},
		{apiURL: "https://api.github.com/", wantAuth: "https://github.com/login/oauth/authorize", wantToken: "https://github.com/login/oauth/access_token"},
		{apiURL: "https://ghe.example.com/api/v3", wantAuth: "https://ghe.example.com/login/oauth/authorize", wantToken: "https://ghe.example.com/login/oauth/access_token"},
		{apiURL: "http://127.0.0.1:8080", wantAuth: "http://127.0.0.1:8080/login/oauth/authorize", wantToken: "http://127.0.0.1:8080/login/oauth/access_token"},
		{apiURL: "http://ghe.example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.apiURL, func(t *testing.T) {
			t.Parallel()
			authURL, tokenURL, err := OAuthEndpoints(tt.apiURL)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidBaseURL) {
					t.Errorf("error = %v, want ErrInvalidBaseURL", err)
				}
				return
			}
			if err != nil || authURL != tt.wantAuth || tokenURL != tt.wantToken {
				t.Errorf("OAuthEndpoints(%q) = %q, %q, %v", tt.apiURL, authURL, tokenURL, err)
			}
		})
	}
}
