package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

type headerRecorder struct {
	mu      sync.Mutex
	headers []http.Header
}

func (h *headerRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.headers = append(h.headers, r.Header.Clone())
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (h *headerRecorder) last(t *testing.T) http.Header {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.headers) == 0 {
		t.Fatal("no request recorded")
	}
	return h.headers[len(h.headers)-1]
}

func TestTransportHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		accept     string
		wantAccept string
	}{
		{name: "default accept", wantAccept: "application/vnd.github+json"},
		{name: "caller accept preserved", accept: "application/vnd.github.raw+json", wantAccept: "application/vnd.github.raw+json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 7)
			rec := &headerRecorder{}
			f.srv.Handle("GET /repos/{owner}/{repo}/thing", rec)
			client := f.repoClient(t, "acme", "app")

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.srv.URL+"/repos/acme/app/thing", http.NoBody)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Authorization", "Bearer caller")
			if tt.accept != "" {
				req.Header.Set("Accept", tt.accept)
			}
			before := req.Header.Clone()
			resp, err := client.HTTPClient().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d", resp.StatusCode)
			}

			got := rec.last(t)
			if auth := got.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ghs_") {
				t.Errorf("Authorization = %q, want an installation token", auth)
			}
			want := map[string]string{
				"Accept":               tt.wantAccept,
				"X-GitHub-Api-Version": "2022-11-28",
				"User-Agent":           "doorstep/test",
			}
			for k, v := range want {
				if got.Get(k) != v {
					t.Errorf("%s = %q, want %q", k, got.Get(k), v)
				}
			}
			if len(req.Header) != len(before) || req.Header.Get("Authorization") != "Bearer caller" || req.Header.Get("X-GitHub-Api-Version") != "" {
				t.Errorf("caller request mutated: %v", req.Header)
			}
		})
	}
}

func TestTransportRefusesForeignHosts(t *testing.T) {
	t.Parallel()

	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("foreign host received Authorization %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(foreign.Close)

	tests := []struct {
		name string
		path string
	}{
		{name: "direct request", path: foreign.URL + "/steal"},
		{name: "redirect", path: "redirect"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 7)
			f.srv.Handle("GET /repos/{owner}/{repo}/redirect", http.RedirectHandler(foreign.URL+"/steal", http.StatusFound))
			client := f.repoClient(t, "acme", "app")

			target := tt.path
			if target == "redirect" {
				target = f.srv.URL + "/repos/acme/app/redirect"
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := client.HTTPClient().Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, ErrForeignHost) {
				t.Errorf("Do error = %v, want ErrForeignHost", err)
			}
		})
	}
	t.Cleanup(func() {
		if got := foreignCalls.Load(); got != 0 {
			t.Errorf("foreign host received %d requests", got)
		}
	})
}

func TestAppRequestsRefuseForeignRedirects(t *testing.T) {
	t.Parallel()

	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(foreign.Close)

	tests := []struct {
		name  string
		route string
		call  func(ctx context.Context, app *App) error
	}{
		{
			name:  "installation lookup",
			route: "GET /repos/acme/app/installation",
			call: func(ctx context.Context, app *App) error {
				_, err := app.installationID(ctx, "acme", "app")
				return err
			},
		},
		{
			name:  "token mint",
			route: "POST /app/installations/7/access_tokens",
			call: func(ctx context.Context, app *App) error {
				_, err := app.installationToken(ctx, 7)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path != tt.route {
					http.NotFound(w, r)
					return
				}
				http.Redirect(w, r, foreign.URL+"/steal", http.StatusTemporaryRedirect)
			}))
			t.Cleanup(upstream.Close)
			app, err := New(Options{AppID: "1", PrivateKey: generateRSAKey(t, 2048), BaseURL: upstream.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := tt.call(t.Context(), app); !errors.Is(err, ErrForeignHost) {
				t.Errorf("error = %v, want ErrForeignHost", err)
			}
		})
	}
	t.Cleanup(func() {
		if got := foreignCalls.Load(); got != 0 {
			t.Errorf("foreign host received %d requests", got)
		}
	})
}

func TestTransportSchemeMismatchRefusedBeforeMinting(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	client := f.repoClient(t, "acme", "app")
	httpsURL := "https" + strings.TrimPrefix(f.srv.URL, "http") + "/repos/acme/app"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, httpsURL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.HTTPClient().Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrForeignHost) {
		t.Errorf("Do error = %v, want ErrForeignHost", err)
	}
	if got := f.srv.TokenCalls(); got != 0 {
		t.Errorf("token calls = %d, want 0", got)
	}
}

func TestTransportUnauthorizedDropsToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	f.srv.Handle("GET /repos/{owner}/{repo}/thing", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := githubtest.InstallationID(r)
		githubtest.WriteJSON(w, http.StatusOK, map[string]int64{"installation": id})
	}))
	client := f.repoClient(t, "acme", "app")

	var out struct {
		Installation int64 `json:"installation"`
	}
	if err := client.Do(t.Context(), http.MethodGet, "/repos/acme/app/thing", nil, &out); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if out.Installation != 7 {
		t.Errorf("installation = %d, want 7", out.Installation)
	}

	f.srv.RevokeTokens(7)
	err := client.Do(t.Context(), http.MethodGet, "/repos/acme/app/thing", nil, &out)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked request error = %v, want 401 APIError", err)
	}

	if err := client.Do(t.Context(), http.MethodGet, "/repos/acme/app/thing", nil, &out); err != nil {
		t.Fatalf("request after 401: %v", err)
	}
	if got := f.srv.TokenCalls(); got != 2 {
		t.Errorf("token calls = %d, want 2", got)
	}
}
