package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
)

type promHeaders struct {
	mu      sync.Mutex
	headers map[string]http.Header
}

func (p *promHeaders) get(path string) http.Header {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.headers[path]
}

func newPromServer(t *testing.T) (*promHeaders, *httptest.Server) {
	t.Helper()
	seen := &promHeaders{headers: map[string]http.Header{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		seen.headers[r.URL.Path] = r.Header.Clone()
		seen.mu.Unlock()
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1764000000,"0"]}]}}`)
	}))
	t.Cleanup(srv.Close)
	return seen, srv
}

func TestRegisterPrometheusGate(t *testing.T) {
	t.Parallel()

	seen, srv := newPromServer(t)
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("file-password\n"), 0o600); err != nil {
		t.Fatalf("write password: %v", err)
	}
	gate := func(name, server string) config.Gate {
		return config.Gate{Type: config.GatePrometheus, Name: name, Server: server, Query: "up", Condition: "> -1"}
	}
	catalog := config.File{
		Prometheus: []config.PrometheusServer{
			{Name: "bearer", URL: srv.URL + "/bearer", BearerTokenEnv: "PROM_TOKEN"},
			{Name: "basic", URL: srv.URL + "/basic", Username: "doorstep", PasswordFile: passwordFile},
			{Name: "open", URL: srv.URL + "/open"},
		},
		Apps: []config.App{{
			Name:   "api",
			Source: config.Source{Repo: "example-org/api"},
			Image:  config.Image{Repository: "ghcr.io/example-org/api"},
			GitOps: config.GitOps{Repo: "example-org/platform", Path: "apps/api"},
			Gates: config.Gates{Before: []config.Gate{}, After: []config.Gate{
				gate("bearer", "bearer"), gate("basic", "basic"), gate("open", "open"),
			}},
		}},
	}.Catalog()
	if err := catalog.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	env := &mutableEnv{vars: map[string]string{"PROM_TOKEN": "env-token"}}
	r := gates.NewRegistry()
	if err := registerPrometheusGate(r, catalog, secretSource{lookupEnv: env.lookup, readFile: config.ReadSecretFile}); err != nil {
		t.Fatalf("registerPrometheusGate: %v", err)
	}
	plans, err := r.Plans(catalog)
	if err != nil {
		t.Fatalf("Plans: %v", err)
	}
	for _, b := range plans["api"].After {
		ev, err := b.Gate.Evaluate(t.Context(), gates.Request{Deployment: core.Deployment{ID: 1}, Phase: core.GatePhaseAfter})
		if err != nil || ev.Status != core.GatePending {
			t.Fatalf("gate %s: %+v, %v", b.Name, ev, err)
		}
	}

	tests := []struct {
		path string
		want string
	}{
		{path: "/bearer/api/v1/query", want: "Bearer env-token"},
		{path: "/basic/api/v1/query", want: "Basic ZG9vcnN0ZXA6ZmlsZS1wYXNzd29yZA=="},
		{path: "/open/api/v1/query", want: ""},
	}
	for _, tt := range tests {
		h := seen.get(tt.path)
		if h == nil {
			t.Errorf("no request reached %s", tt.path)
			continue
		}
		if got := h.Get("Authorization"); got != tt.want {
			t.Errorf("%s Authorization = %q, want %q", tt.path, got, tt.want)
		}
		if got := h.Get("User-Agent"); !strings.HasPrefix(got, "doorstep/") {
			t.Errorf("%s User-Agent = %q", tt.path, got)
		}
	}

	env.set("PROM_TOKEN", "rotated")
	ev, err := plans["api"].After[0].Gate.Evaluate(t.Context(), gates.Request{Deployment: core.Deployment{ID: 2}, Phase: core.GatePhaseAfter})
	if err != nil || ev.Status != core.GatePending {
		t.Fatalf("rotated: %+v, %v", ev, err)
	}
	if got := seen.get("/bearer/api/v1/query").Get("Authorization"); got != "Bearer rotated" {
		t.Errorf("Authorization after rotation = %q", got)
	}
}

func TestRegisterPrometheusGateErrors(t *testing.T) {
	t.Parallel()

	secrets := secretSource{lookupEnv: (&mutableEnv{}).lookup, readFile: config.ReadSecretFile}
	bad := config.Catalog{Prometheus: []config.PrometheusServer{{Name: "main", URL: "ftp://prom"}}}
	if err := registerPrometheusGate(gates.NewRegistry(), bad, secrets); err == nil || !strings.Contains(err.Error(), "prometheus main:") {
		t.Errorf("error = %v", err)
	}
	r := gates.NewRegistry()
	if err := registerPrometheusGate(r, config.Catalog{}, secrets); err != nil {
		t.Fatalf("registerPrometheusGate without servers: %v", err)
	}
	if err := registerPrometheusGate(r, config.Catalog{}, secrets); err == nil {
		t.Error("registerPrometheusGate registered the type twice")
	}
}
