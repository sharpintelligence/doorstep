package cli

import (
	"bytes"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/argocd"
	"github.com/sharpintelligence/doorstep/internal/argocd/argocdtest"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/kube"
)

const (
	argoPinSHA   = "1111111111111111111111111111111111111111"
	argoOtherSHA = "2222222222222222222222222222222222222222"
)

const argoConfig = `
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
  - name: shop
    source:
      repo: example-user/shop
    image:
      repository: ghcr.io/example-user/shop
    gitops:
      repo: example-user/homelab
      path: apps/shop
    argocd:
      application: shop-prod
      namespace: apps
      sync_timeout: 20m
`

func TestArgoCDStatusCommand(t *testing.T) {
	t.Parallel()

	degraded := argocdtest.Synced("argocd", "blog", "https://github.com/example-user/homelab", argoPinSHA)
	degraded.Health = "Degraded"
	degraded.HealthMessage = "one pod failing"
	degraded.Resources = []argocdtest.Resource{
		{Group: "apps", Kind: "Deployment", Namespace: "blog", Name: "web", Status: "Synced", Health: "Degraded", HealthMessage: "crash loop"},
	}

	tests := []struct {
		name       string
		app        *argocdtest.Application
		fail       int
		args       []string
		env        map[string]string
		noKube     bool
		wantCode   int
		wantOut    []string
		wantNotOut []string
		wantErr    string
	}{
		{
			name: "healthy",
			app:  new(argocdtest.Synced("argocd", "blog", "https://github.com/example-user/homelab", argoPinSHA)),
			args: []string{"blog"},
			wantOut: []string{
				"application  argocd/blog\n",
				"sync         synced\n",
				"revision     " + argoPinSHA + "\n",
				"operation    Succeeded: successfully synced",
				"health       healthy\n",
			},
			wantNotOut: []string{"reached", "resources"},
		},
		{
			name: "degraded with a revision",
			app:  &degraded,
			args: []string{"blog", "--revision", argoPinSHA},
			wantOut: []string{
				"health       degraded: one pod failing\n",
				"pin          " + argoPinSHA + "\n",
				"reached      yes\n",
				"ready        no\n",
				"reason       degraded (version): application health is degraded: one pod failing",
				"resources\n  apps/Deployment blog/web: degraded, Synced: crash loop\n",
			},
		},
		{
			name: "revision not reached without github",
			app:  new(argocdtest.Synced("argocd", "blog", "https://github.com/example-user/homelab", argoOtherSHA)),
			args: []string{"blog", "--revision", argoPinSHA},
			wantOut: []string{
				"reached      no\n",
				"reason       not_observed (transient): synced revision " + argoOtherSHA + " does not contain " + argoPinSHA + " yet",
			},
		},
		{
			name:     "kubeconfig from the environment",
			app:      new(argocdtest.Synced("argocd", "blog", "https://github.com/example-user/homelab", argoPinSHA)),
			args:     []string{"blog"},
			env:      map[string]string{config.EnvKubeconfig: "<kubeconfig>"},
			noKube:   true,
			wantCode: 0,
			wantOut:  []string{"sync         synced\n"},
		},
		{
			name:     "unknown app",
			args:     []string{"wiki"},
			wantCode: 1,
			wantErr:  `unknown app "wiki" (configured: blog, shop)`,
		},
		{
			name:     "application missing",
			args:     []string{"shop"},
			wantCode: 1,
			wantErr:  "application apps/shop-prod: sync target not found",
		},
		{
			name:     "forbidden",
			app:      new(argocdtest.Synced("argocd", "blog", "https://github.com/example-user/homelab", argoPinSHA)),
			fail:     http.StatusForbidden,
			args:     []string{"blog"},
			wantCode: 1,
			wantErr:  "kube api: 403 Forbidden",
		},
		{
			name:     "invalid revision",
			app:      new(argocdtest.Synced("argocd", "blog", "https://github.com/example-user/homelab", argoPinSHA)),
			args:     []string{"blog", "--revision", "main"},
			wantCode: 1,
			wantErr:  "invalid commit SHA",
		},
		{
			name:     "not in a cluster",
			args:     []string{"blog"},
			noKube:   true,
			wantCode: 1,
			wantErr:  "set " + config.EnvKubeconfig + " or pass --kubeconfig",
		},
		{
			name:     "context without kubeconfig",
			args:     []string{"blog", "--kube-context", "lab"},
			noKube:   true,
			wantCode: 1,
			wantErr:  `kubernetes context: "lab" needs a kubeconfig`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := argocdtest.NewServer(t)
			if tt.app != nil {
				srv.Set(*tt.app)
			}
			if tt.fail != 0 {
				srv.FailNext(argocdtest.Get, tt.fail, -1)
			}
			kubeconfig := srv.Kubeconfig(t)
			args := append([]string{"argocd", "status", "--config", writeConfig(t, argoConfig)}, tt.args...)
			if !tt.noKube {
				args = append(args, "--kubeconfig", kubeconfig)
			}
			env := map[string]string{}
			for k, v := range tt.env {
				env[k] = strings.ReplaceAll(v, "<kubeconfig>", kubeconfig)
			}
			code, stdout, stderr := runCLI(t, args, env)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; stdout %q, stderr %q", code, tt.wantCode, stdout, stderr)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout = %q, want it to contain %q", stdout, want)
				}
			}
			for _, unwanted := range tt.wantNotOut {
				if strings.Contains(stdout, unwanted) {
					t.Errorf("stdout = %q, must not contain %q", stdout, unwanted)
				}
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}
		})
	}
}

func TestSyncApplications(t *testing.T) {
	t.Parallel()

	file, err := config.Parse(strings.NewReader(argoConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := syncApplications(file.Catalog())
	want := map[core.Target]argocd.Application{
		core.AppTarget("blog"): {Namespace: "argocd", Name: "blog", Repository: "example-user/homelab", SyncTimeout: core.DefaultSyncTimeout},
		core.AppTarget("shop"): {Namespace: "apps", Name: "shop-prod", Repository: "example-user/homelab", SyncTimeout: 20 * time.Minute},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("syncApplications() = %+v, want %+v", got, want)
	}
}

func TestGitHubAncestry(t *testing.T) {
	t.Parallel()

	srv := githubtest.NewServer(t)
	fake := srv.AddRepo("example-user", "homelab", 5)
	first := fake.Seed("main", "a.yaml", "a: 1\n")
	fake.CreateBranch("side", "main")
	second := fake.Commit("main", "second", map[string]string{"a.yaml": "a: 2\n"})
	side := fake.Commit("side", "side", map[string]string{"b.yaml": "b: 1\n"})
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}

	tests := []struct {
		name                 string
		repository           string
		ancestor, descendant string
		want                 bool
		wantErr              string
		wantWarning          bool
	}{
		{name: "later commit", repository: "example-user/homelab", ancestor: first, descendant: second, want: true},
		{name: "same commit", repository: "example-user/homelab", ancestor: second, descendant: second, want: true},
		{name: "older commit", repository: "example-user/homelab", ancestor: second, descendant: first},
		{name: "diverged", repository: "example-user/homelab", ancestor: second, descendant: side},
		{name: "unknown commit", repository: "example-user/homelab", ancestor: first, descendant: strings.Repeat("e", 40), wantWarning: true},
		{name: "malformed repository", repository: "homelab", ancestor: first, descendant: second, wantErr: "must be owner/name"},
		{name: "not installed", repository: "example-user/other", ancestor: first, descendant: second, wantErr: "not installed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			ancestry := githubAncestry{repos: app, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			got, err := ancestry.Contains(t.Context(), tt.repository, tt.ancestor, tt.descendant)
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tt.wantWarning {
				t.Errorf("warning logged = %v, want %v: %s", warned, tt.wantWarning, logs.String())
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Contains error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Contains = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestNewKubeClient(t *testing.T) {
	t.Parallel()

	srv := argocdtest.NewServer(t)
	kubeconfig := srv.Kubeconfig(t)
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsServer.Close)
	files := map[string][]byte{
		filepath.Join(kube.ServiceAccountDir, "ca.crt"): pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}),
		filepath.Join(kube.ServiceAccountDir, "token"):  []byte("sa-token"),
	}
	readFile := func(name string) ([]byte, error) {
		if b, ok := files[name]; ok {
			return b, nil
		}
		return os.ReadFile(filepath.Clean(name))
	}

	tests := []struct {
		name       string
		cfg        config.Kubernetes
		env        map[string]string
		wantServer string
		wantIs     error
		wantErr    string
	}{
		{name: "kubeconfig", cfg: config.Kubernetes{Kubeconfig: kubeconfig}, wantServer: srv.URL},
		{name: "kubeconfig context", cfg: config.Kubernetes{Kubeconfig: kubeconfig, Context: "test"}, wantServer: srv.URL},
		{name: "unknown context", cfg: config.Kubernetes{Kubeconfig: kubeconfig, Context: "prod"}, wantIs: kube.ErrInvalidConfig},
		{
			name:       "in cluster",
			env:        map[string]string{kube.EnvServiceHost: "10.96.0.1", kube.EnvServicePort: "443"},
			wantServer: "https://10.96.0.1:443",
		},
		{name: "outside a cluster", wantIs: kube.ErrNotInCluster, wantErr: config.EnvKubeconfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, err := newKubeClient(tt.cfg, envFrom(tt.env), readFile)
			if tt.wantIs != nil {
				if !errors.Is(err, tt.wantIs) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("newKubeClient error = %v, want %v containing %q", err, tt.wantIs, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newKubeClient: %v", err)
			}
			if client.Server() != tt.wantServer {
				t.Fatalf("Server() = %q, want %q", client.Server(), tt.wantServer)
			}
		})
	}
}
