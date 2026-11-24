package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

func TestConfigRoleNamesMatchCore(t *testing.T) {
	t.Parallel()

	var roles []string
	for _, r := range core.Roles() {
		roles = append(roles, string(r))
	}
	if !slices.Equal(config.RoleNames(), roles) {
		t.Errorf("config.RoleNames() = %v, core.Roles() = %v", config.RoleNames(), roles)
	}
	if config.ProviderOIDC != "oidc" || config.ProviderGitHub != "github" {
		t.Error("config provider names drifted from the auth package")
	}
}

func fakeDiscovery(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

const authApp = `
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
`

type response struct {
	code    int
	header  http.Header
	cookies []*http.Cookie
	body    string
}

func noRedirectGet(t *testing.T, target string, header http.Header) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return response{code: resp.StatusCode, header: resp.Header, cookies: resp.Cookies(), body: string(body)}
}

func TestRunServeAuthentication(t *testing.T) {
	t.Parallel()

	issuer := fakeDiscovery(t)
	tests := []struct {
		name         string
		config       string
		env          map[string]string
		wantProvider string
		wantLogin    string
	}{
		{
			name:         "tokens only",
			config:       authApp,
			wantProvider: "tokens only",
		},
		{
			name: "oidc",
			config: authApp + `
server:
  public_url: http://127.0.0.1:8080
auth:
  default_role: viewer
  oidc:
    issuer_url: ` + issuer.URL + `
    client_id: doorstep
    client_secret_env: TEST_OIDC_SECRET
`,
			env:          map[string]string{"TEST_OIDC_SECRET": "s3cret-oidc"},
			wantProvider: "oidc",
			wantLogin:    issuer.URL + "/authorize",
		},
		{
			name: "github",
			config: authApp + `
auth:
  github:
    client_id: Iv1.abc
    client_secret_env: TEST_GITHUB_SECRET
  roles:
    admin:
      users: ["id:583231"]
`,
			env:          map[string]string{"TEST_GITHUB_SECRET": "s3cret-github", config.EnvPublicURL: "https://doorstep.example.com"},
			wantProvider: "github",
			wantLogin:    "https://github.com/login/oauth/authorize",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr lockedBuffer
			args, env := withDatabase(t, databaseURLEnv,
				[]string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s", "--config", writeConfig(t, tt.config)}, tt.env)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan int, 1)
			go func() { done <- Run(ctx, args, &stdout, &stderr, envFrom(env)) }()

			addr := waitForListening(t, &stderr, done)
			metricsAddr := waitForMetricsAddr(t, &stderr, done)

			login := noRedirectGet(t, "http://"+addr+"/auth/login?return_to=%2Fqueue", nil)
			if tt.wantLogin == "" {
				if login.code != http.StatusNotFound {
					t.Errorf("GET /auth/login = %d, want 404 without a provider", login.code)
				}
			} else {
				location, err := url.Parse(login.header.Get("Location"))
				if login.code != http.StatusFound || err != nil || !strings.HasPrefix(location.String(), tt.wantLogin+"?") {
					t.Errorf("GET /auth/login = %d Location %q, want a redirect to %s", login.code, login.header.Get("Location"), tt.wantLogin)
				}
				if len(login.cookies) != 1 {
					t.Errorf("login cookies = %v", login.cookies)
				}
			}

			bad := http.Header{"Authorization": {"Bearer dsp_not-a-real-token"}}
			if tt.wantLogin != "" {
				if resp := noRedirectGet(t, "http://"+addr+"/auth/login", bad); resp.code != http.StatusUnauthorized {
					t.Errorf("auth route with a bad token = %d, want 401", resp.code)
				}
			}
			for _, path := range []string{"/healthz", "/readyz"} {
				if resp := noRedirectGet(t, "http://"+addr+path, bad); resp.code != http.StatusOK {
					t.Errorf("GET %s with a bad token = %d, want 200 (built-in endpoints are not authenticated)", path, resp.code)
				}
			}
			if resp := noRedirectGet(t, "http://"+metricsAddr+"/metrics", bad); resp.code != http.StatusOK {
				t.Errorf("metrics listener with a bad token = %d, want 200 (not authenticated)", resp.code)
			}
			if code, _ := get(t, "http://"+addr+"/readyz"); code != http.StatusOK {
				t.Errorf("GET /readyz = %d", code)
			}

			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("exit code = %d, stderr %q", code, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("serve did not exit after cancellation")
			}
			logs := parseLogs(t, stderr.String())
			configured := findMessage(logs, "authentication configured")
			if configured == nil || configured["provider"] != tt.wantProvider {
				t.Errorf("authentication configured log = %v, want provider %q", configured, tt.wantProvider)
			}
			if findMessage(logs, "identity provider unreachable") != nil {
				t.Errorf("provider probe failed: %s", stderr.String())
			}
			if strings.Contains(stderr.String(), "s3cret") {
				t.Errorf("logs leak a client secret: %s", stderr.String())
			}
		})
	}
}

func TestRunServeWarnsWhenIssuerIsDown(t *testing.T) {
	t.Parallel()

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	cfg := authApp + `
server:
  public_url: http://localhost:8080
auth:
  default_role: viewer
  oidc:
    issuer_url: ` + down.URL + `
    client_id: doorstep
    client_secret_env: TEST_OIDC_SECRET
`
	var stderr lockedBuffer
	args, env := withDatabase(t, databaseURLEnv,
		[]string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "", "--shutdown-delay", "0s", "--config", writeConfig(t, cfg)},
		map[string]string{"TEST_OIDC_SECRET": "x"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- Run(ctx, args, io.Discard, &stderr, envFrom(env)) }()
	addr := waitForListening(t, &stderr, done)
	waitForLog(t, &stderr, done, "identity provider unreachable")

	if resp := noRedirectGet(t, "http://"+addr+"/auth/login", nil); resp.code != http.StatusServiceUnavailable {
		t.Errorf("GET /auth/login with the issuer down = %d, want 503", resp.code)
	}
	bad := http.Header{"Authorization": {"Bearer dsp_stale"}}
	if resp := noRedirectGet(t, "http://"+addr+"/metrics", bad); resp.code != http.StatusOK {
		t.Errorf("GET /metrics on the main listener with a stale token = %d, want 200", resp.code)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit code = %d, stderr %q", code, stderr.String())
	}
}

func TestServeAndValidateRequirePublicURLForSignIn(t *testing.T) {
	t.Parallel()

	cfg := authApp + `
auth:
  github:
    client_id: Iv1.abc
    client_secret_env: TEST_GITHUB_SECRET
`
	path := writeConfig(t, cfg)
	code, _, stderr := cliRun{t: t}.run("config", "validate", "--config", path)
	if code != 1 || !strings.Contains(stderr, "server.public_url: required when a sign-in provider is configured") {
		t.Errorf("config validate = %d %q", code, stderr)
	}
	code, _, stderr = cliRun{t: t, env: map[string]string{config.EnvDatabaseURL: "postgres://unused"}}.run("serve", "--config", path)
	if code != 1 || !strings.Contains(stderr, "server.public_url") {
		t.Errorf("serve = %d %q", code, stderr)
	}

	withURL := writeConfig(t, cfg+"server:\n  public_url: https://doorstep.example.com\n")
	stdout, _ := cliRun{t: t}.mustRun("config", "validate", "--config", withURL)
	if stdout != "ok: 1 apps, 0 notification targets\n" {
		t.Errorf("config validate stdout = %q", stdout)
	}
	code, _, stderr = cliRun{t: t, env: map[string]string{config.EnvDatabaseURL: "postgres://unused"}}.run("serve", "--config", path, "--public-url", "http://deploy.example.com")
	if code != 1 || !strings.Contains(stderr, "server.public_url") || !strings.Contains(stderr, "must use https") {
		t.Errorf("serve with plain http public url = %d %q", code, stderr)
	}
}

func TestServeAPIRequiresRoles(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr lockedBuffer
	args, env := withDatabase(t, databaseURLEnv,
		[]string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s", "--config", writeConfig(t, authApp)}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- Run(ctx, args, &stdout, &stderr, envFrom(env)) }()

	addr := waitForListening(t, &stderr, done)
	metricsAddr := waitForMetricsAddr(t, &stderr, done)

	createToken := func(role core.Role) http.Header {
		t.Helper()
		var out, errOut bytes.Buffer
		tokenArgs := []string{"token", "create", "--name", "test-" + string(role), "--role", string(role)}
		if code := Run(t.Context(), tokenArgs, &out, &errOut, envFrom(env)); code != 0 {
			t.Fatalf("token create %s = %d: %s", role, code, errOut.String())
		}
		return http.Header{"Authorization": {"Bearer " + strings.TrimSpace(out.String())}}
	}
	viewer := createToken(core.RoleViewer)
	deployer := createToken(core.RoleDeployer)

	for _, path := range []string{"/api/v1/apps/blog/queue", "/api/v1/locks"} {
		if resp := noRedirectGet(t, "http://"+addr+path, nil); resp.code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, resp.code)
		}
		if resp := noRedirectGet(t, "http://"+addr+path, viewer); resp.code != http.StatusOK {
			t.Errorf("GET %s as viewer = %d, want 200", path, resp.code)
		}
		if resp := noRedirectGet(t, "http://"+metricsAddr+path, viewer); resp.code != http.StatusNotFound {
			t.Errorf("GET %s on the metrics listener = %d, want 404", path, resp.code)
		}
	}

	lockApp := func(header http.Header) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/api/v1/apps/blog/lock", strings.NewReader(`{"reason":"test"}`))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header = header.Clone()
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST lock: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := lockApp(viewer); code != http.StatusForbidden {
		t.Errorf("lock as viewer = %d, want 403", code)
	}
	if code := lockApp(deployer); code != http.StatusCreated {
		t.Errorf("lock as deployer = %d, want 201", code)
	}
	if resp := noRedirectGet(t, "http://"+addr+"/api/v1/locks", viewer); !strings.Contains(resp.body, `"owner":"token:test-deployer"`) {
		t.Errorf("lock owner is not the token actor: %s", resp.body)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, stderr %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after cancellation")
	}
}
