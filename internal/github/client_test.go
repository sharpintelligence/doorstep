package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
)

type widget struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestClientDo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		method  string
		path    string
		in      any
		out     bool
		handler http.HandlerFunc
		want    widget
		wantErr func(t *testing.T, err error)
	}{
		{
			name:   "json round trip",
			method: http.MethodPost,
			path:   "/repos/acme/app/widgets",
			in:     widget{Name: "bolt", Count: 2},
			out:    true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Content-Type") != "application/json" {
					githubtest.WriteError(w, http.StatusUnsupportedMediaType, "bad content type")
					return
				}
				var in widget
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					githubtest.WriteError(w, http.StatusBadRequest, err.Error())
					return
				}
				in.Count *= 10
				githubtest.WriteJSON(w, http.StatusCreated, in)
			},
			want: widget{Name: "bolt", Count: 20},
		},
		{
			name:   "query string preserved",
			method: http.MethodGet,
			path:   "/repos/acme/app/widgets?ref=main&per_page=1",
			out:    true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				githubtest.WriteJSON(w, http.StatusOK, widget{Name: r.URL.Query().Get("ref"), Count: len(r.URL.Query())})
			},
			want: widget{Name: "main", Count: 2},
		},
		{
			name:   "no content with nil out",
			method: http.MethodDelete,
			path:   "/repos/acme/app/widgets",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.ContentLength > 0 || r.Header.Get("Content-Type") != "" {
					githubtest.WriteError(w, http.StatusBadRequest, "unexpected body")
					return
				}
				w.WriteHeader(http.StatusNoContent)
			},
		},
		{
			name:   "no content with out",
			method: http.MethodPut,
			path:   "/repos/acme/app/widgets",
			in:     widget{Name: "nut"},
			out:    true,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
		},
		{
			name:   "body ignored with nil out",
			method: http.MethodGet,
			path:   "/repos/acme/app/widgets",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				githubtest.WriteJSON(w, http.StatusOK, widget{Name: "ignored"})
			},
		},
		{
			name:   "api error",
			method: http.MethodPut,
			path:   "/repos/acme/app/widgets",
			in:     widget{},
			out:    true,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				githubtest.WriteError(w, http.StatusConflict, "sha does not match")
			},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("error %v is not an APIError", err)
				}
				want := APIError{StatusCode: http.StatusConflict, Message: "sha does not match", RequestID: "FAKE:409"}
				if *apiErr != want {
					t.Errorf("APIError = %+v, want %+v", *apiErr, want)
				}
				if got := err.Error(); got != "github api: 409 Conflict: sha does not match (request id FAKE:409)" {
					t.Errorf("Error() = %q", got)
				}
			},
		},
		{
			name:   "api error without json body",
			method: http.MethodGet,
			path:   "/repos/acme/app/widgets",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "upstream exploded", http.StatusBadGateway)
			},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway || apiErr.Message != "" {
					t.Fatalf("error = %#v", err)
				}
				if got := err.Error(); got != "github api: 502 Bad Gateway" {
					t.Errorf("Error() = %q", got)
				}
			},
		},
		{
			name:   "malformed response",
			method: http.MethodGet,
			path:   "/repos/acme/app/widgets",
			out:    true,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("<html>"))
			},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "decode GET /repos/acme/app/widgets response") {
					t.Errorf("error = %v", err)
				}
			},
		},
		{
			name:   "oversized response is bounded",
			method: http.MethodGet,
			path:   "/repos/acme/app/widgets",
			out:    true,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"name":"`))
				chunk := []byte(strings.Repeat("a", 1<<20))
				for range 11 {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
				_, _ = w.Write([]byte(`"}`))
			},
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
					t.Errorf("error = %v, want unexpected EOF", err)
				}
			},
		},
		{
			name:    "unencodable input",
			method:  http.MethodPost,
			path:    "/repos/acme/app/widgets",
			in:      map[string]any{"bad": make(chan int)},
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			wantErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "encode POST /repos/acme/app/widgets request") {
					t.Errorf("error = %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.Install("acme", "app", 7)
			f.srv.Handle("/repos/{owner}/{repo}/widgets", tt.handler)
			client := f.repoClient(t, "acme", "app")

			var got widget
			var out any
			if tt.out {
				out = &got
			}
			err := client.Do(t.Context(), tt.method, tt.path, tt.in, out)
			if tt.wantErr != nil {
				tt.wantErr(t, err)
				return
			}
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if got != tt.want {
				t.Errorf("out = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestClientEnterpriseBasePath(t *testing.T) {
	t.Parallel()

	srv := githubtest.NewServer(t)
	clock := newFakeClock()
	srv.SetNow(clock.Now)
	srv.Install("acme", "app", 7)
	srv.Handle("GET /repos/{owner}/{repo}/widgets", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		githubtest.WriteJSON(w, http.StatusOK, widget{Name: r.PathValue("owner") + "/" + r.PathValue("repo")})
	}))
	ghe := httptest.NewServer(http.StripPrefix("/api/v3", srv))
	t.Cleanup(ghe.Close)

	app := newApp(t, srv, ghe.URL+"/api/v3/", clock)
	client, err := app.RepoClient(t.Context(), "acme", "app")
	if err != nil {
		t.Fatalf("RepoClient: %v", err)
	}
	var got widget
	if err := client.Do(t.Context(), http.MethodGet, "/repos/acme/app/widgets", nil, &got); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got.Name != "acme/app" {
		t.Errorf("name = %q", got.Name)
	}
}

func TestClientContextCancellation(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	entered := make(chan struct{})
	f.srv.Handle("GET /repos/{owner}/{repo}/slow", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	client := f.repoClient(t, "acme", "app")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- client.Do(ctx, http.MethodGet, "/repos/acme/app/slow", nil, nil) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Do error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do did not return after cancellation")
	}
}

func TestClientUsesInjectedHTTPClient(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.srv.Install("acme", "app", 7)
	f.srv.Handle("GET /repos/{owner}/{repo}/widgets", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	var seen []string
	base := &http.Client{
		Timeout: 7 * time.Second,
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = append(seen, r.Method+" "+r.URL.Path)
			return http.DefaultTransport.RoundTrip(r)
		}),
	}
	app, err := New(Options{AppID: f.srv.AppID, PrivateKey: f.srv.PrivateKey, BaseURL: f.srv.URL, HTTPClient: base, Now: f.clock.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, err := app.RepoClient(t.Context(), "acme", "app")
	if err != nil {
		t.Fatalf("RepoClient: %v", err)
	}
	if client.HTTPClient().Timeout != 7*time.Second {
		t.Errorf("timeout = %s, want 7s", client.HTTPClient().Timeout)
	}
	if err := client.Do(t.Context(), http.MethodGet, "/repos/acme/app/widgets", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	want := []string{
		"GET /repos/acme/app/installation",
		"POST /app/installations/7/access_tokens",
		"GET /repos/acme/app/widgets",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests = %q, want %q", seen, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
