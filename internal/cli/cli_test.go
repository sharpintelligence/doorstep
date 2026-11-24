package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envFrom(vars map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const oneAppConfig = `
log:
  level: debug
notifications:
  - name: ops
    type: slack
    webhook_url_env: TEST_SLACK_URL
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    notify: [ops]
`

const invalidAppsConfig = `
apps:
  - name: Blog
    source:
      repo: blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: /apps/blog
    notify: [pager]
`

func TestRunVersion(t *testing.T) {
	t.Parallel()

	want := buildinfo.Read()
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, stdout string)
	}{
		{
			name: "text",
			args: []string{"version"},
			check: func(t *testing.T, stdout string) {
				t.Helper()
				if stdout != want.String()+"\n" {
					t.Errorf("stdout = %q, want %q", stdout, want.String()+"\n")
				}
			},
		},
		{
			name: "json",
			args: []string{"version", "--json"},
			check: func(t *testing.T, stdout string) {
				t.Helper()
				var got buildinfo.Info
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("Unmarshal %q: %v", stdout, err)
				}
				if got != want {
					t.Errorf("version JSON = %+v, want %+v", got, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := Run(t.Context(), tt.args, &stdout, &stderr, envFrom(nil))
			if code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("unexpected stderr %q", stderr.String())
			}
			tt.check(t, stdout.String())
		})
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()

	const password = "hunter2-very-secret"
	tests := []struct {
		name         string
		args         []string
		env          map[string]string
		config       string
		wantStderr   []string
		forbidStderr []string
	}{
		{
			name:       "unknown command",
			args:       []string{"frobnicate"},
			wantStderr: []string{`doorstep: unknown command "frobnicate"`},
		},
		{
			name:       "unknown flag",
			args:       []string{"serve", "--nope"},
			wantStderr: []string{"doorstep: unknown flag: --nope"},
		},
		{
			name:       "bad duration flag",
			args:       []string{"serve", "--shutdown-timeout", "soon"},
			wantStderr: []string{"--shutdown-timeout", `"soon"`},
		},
		{
			name:       "unexpected argument",
			args:       []string{"version", "extra"},
			wantStderr: []string{"doorstep: unknown command \"extra\""},
		},
		{
			name: "invalid configuration reports every error",
			args: []string{"serve", "--listen-addr", "nowhere", "--log-level", "loud", "--log-format", "xml", "--shutdown-delay", "-1s", "--shutdown-timeout", "0s"},
			wantStderr: []string{
				"doorstep: invalid configuration",
				"listen address",
				"shutdown delay",
				"shutdown timeout",
				"log level",
				"log format",
				"database",
			},
		},
		{
			name:       "invalid environment",
			args:       []string{"serve"},
			env:        map[string]string{config.EnvShutdownTimeout: "later"},
			wantStderr: []string{"doorstep: invalid environment", config.EnvShutdownTimeout},
		},
		{
			name:       "invalid value from environment",
			args:       []string{"serve"},
			env:        map[string]string{config.EnvLogFormat: "xml"},
			wantStderr: []string{"doorstep: invalid configuration", "log format"},
		},
		{
			name:       "serve without database",
			args:       []string{"serve"},
			wantStderr: []string{"doorstep: invalid configuration", config.EnvDatabaseURL, config.EnvDatabaseURLFile},
		},
		{
			name: "serve with both database sources",
			args: []string{"serve"},
			env: map[string]string{
				config.EnvDatabaseURL:     "postgres://doorstep:" + password + "@localhost/doorstep",
				config.EnvDatabaseURLFile: "/run/secrets/database-url",
			},
			wantStderr:   []string{"doorstep: invalid configuration", "exactly one"},
			forbidStderr: []string{password},
		},
		{
			name:       "serve with missing database URL file",
			args:       []string{"serve", "--database-url-file", "/nonexistent/database-url"},
			wantStderr: []string{"doorstep: read database URL file", "/nonexistent/database-url"},
		},
		{
			name:         "serve with unreachable database",
			args:         []string{"serve"},
			env:          map[string]string{config.EnvDatabaseURL: "postgres://doorstep:" + password + "@127.0.0.1:1/doorstep?connect_timeout=1&sslmode=disable"},
			wantStderr:   []string{"doorstep: open database: connect to database"},
			forbidStderr: []string{password, "postgres://"},
		},
		{
			name:         "serve with malformed database URL",
			args:         []string{"serve"},
			env:          map[string]string{config.EnvDatabaseURL: "postgres://doorstep:" + password + "@localhost:notaport/doorstep"},
			wantStderr:   []string{"doorstep: open database: invalid database URL"},
			forbidStderr: []string{password, "postgres://"},
		},
		{
			name:       "invalid metrics listen address",
			args:       []string{"serve", "--metrics-listen-addr", "nope"},
			wantStderr: []string{"doorstep: invalid configuration", "metrics listen address"},
		},
		{
			name:       "invalid metrics listen address from environment",
			args:       []string{"serve"},
			env:        map[string]string{config.EnvMetricsListenAddr: ":http"},
			wantStderr: []string{"doorstep: invalid configuration", "metrics listen address"},
		},
		{
			name:       "migrate without database",
			args:       []string{"migrate"},
			wantStderr: []string{"doorstep: invalid configuration: database", config.EnvDatabaseURL},
		},
		{
			name:       "migrate status without database",
			args:       []string{"migrate", "status"},
			wantStderr: []string{"doorstep: invalid configuration: database"},
		},
		{
			name:       "migrate with invalid log level",
			args:       []string{"migrate", "--log-level", "loud", "--database-url-file", "/run/secrets/database-url"},
			wantStderr: []string{"doorstep: invalid configuration", "log level"},
		},
		{
			name:       "migrate ignores server settings",
			args:       []string{"migrate", "--database-url-file", "/nonexistent/database-url"},
			env:        map[string]string{config.EnvListenAddr: "nowhere"},
			wantStderr: []string{"doorstep: read database URL file"},
		},
		{
			name:       "migrate reads the database URL file from the config file",
			args:       []string{"migrate"},
			config:     "database:\n  url_file: /nonexistent/file-database-url\n",
			wantStderr: []string{"doorstep: read database URL file", "/nonexistent/file-database-url"},
		},
		{
			name:       "migrate database environment over config file",
			args:       []string{"migrate", "status"},
			env:        map[string]string{config.EnvDatabaseURLFile: "/nonexistent/env-database-url"},
			config:     "database:\n  url_file: /nonexistent/file-database-url\n",
			wantStderr: []string{"doorstep: read database URL file", "/nonexistent/env-database-url"},
		},
		{
			name:       "image resolve reads registry credentials from the config file",
			args:       []string{"image", "resolve", "ghcr.io/owner/app", "v1"},
			config:     "registry:\n  credentials_file: /nonexistent/doorstep/config.json\n",
			wantStderr: []string{"doorstep: registry credentials", "/nonexistent/doorstep/config.json"},
		},
		{
			name:       "serve reads github settings from the config file",
			args:       []string{"serve"},
			env:        map[string]string{config.EnvDatabaseURL: "postgres://doorstep@localhost/doorstep"},
			config:     "github:\n  app_id: \"123\"\n",
			wantStderr: []string{"doorstep: invalid configuration", "github app private key: required"},
		},
		{
			name:       "migrate rejects server flags",
			args:       []string{"migrate", "--listen-addr", ":0"},
			wantStderr: []string{"doorstep: unknown flag: --listen-addr"},
		},
		{
			name:       "migrate unexpected argument",
			args:       []string{"migrate", "down"},
			wantStderr: []string{`doorstep: unknown command "down"`},
		},
		{
			name:         "migrate status with unreachable database",
			args:         []string{"migrate", "status"},
			env:          map[string]string{config.EnvDatabaseURL: "postgres://doorstep:" + password + "@127.0.0.1:1/doorstep?connect_timeout=1&sslmode=disable"},
			wantStderr:   []string{"doorstep: open database"},
			forbidStderr: []string{password, "postgres://"},
		},
		{
			name:       "missing registry credentials file",
			args:       []string{"serve", "--listen-addr", "127.0.0.1:0", "--registry-credentials-file", "/nonexistent/doorstep/config.json"},
			env:        map[string]string{config.EnvDatabaseURL: "postgres://doorstep@localhost/doorstep"},
			wantStderr: []string{"doorstep: registry credentials: read registry credentials file", "/nonexistent/doorstep/config.json"},
		},
		{
			name: "malformed registry credentials file from environment",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0"},
			env: map[string]string{
				config.EnvDatabaseURL:             "postgres://doorstep@localhost/doorstep",
				config.EnvRegistryCredentialsFile: "testdata/malformed-dockerconfig.json",
			},
			wantStderr: []string{"doorstep: registry credentials: parse registry credentials file", "no usable credential"},
		},
		{
			name: "incomplete github configuration",
			args: []string{"serve"},
			env: map[string]string{
				config.EnvDatabaseURL: "postgres://doorstep@localhost/doorstep",
				config.EnvGitHubAppID: "123",
			},
			wantStderr: []string{"doorstep: invalid configuration", "github app private key: required"},
		},
		{
			name: "malformed github private key fails startup",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0"},
			env: map[string]string{
				config.EnvDatabaseURL:         "postgres://doorstep@localhost/doorstep",
				config.EnvGitHubAppID:         "123",
				config.EnvGitHubAppPrivateKey: "not-a-pem-supersecret",
			},
			wantStderr: []string{"doorstep: github app private key", "invalid github app private key"},
		},
		{
			name: "missing github private key file fails startup",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0"},
			env: map[string]string{
				config.EnvDatabaseURL:             "postgres://doorstep@localhost/doorstep",
				config.EnvGitHubAppID:             "123",
				config.EnvGitHubAppPrivateKeyFile: "/nonexistent/doorstep/private-key.pem",
			},
			wantStderr: []string{"doorstep: read github app private key", "/nonexistent/doorstep/private-key.pem"},
		},
		{
			name:       "metrics listen address equal to listen address",
			args:       []string{"serve", "--listen-addr", "127.0.0.1:8123", "--metrics-listen-addr", "127.0.0.1:8123"},
			wantStderr: []string{"doorstep: invalid configuration", "metrics listen address", "must differ"},
		},
		{
			name:   "invalid apps report every error",
			args:   []string{"serve"},
			config: invalidAppsConfig,
			wantStderr: []string{
				"doorstep: invalid configuration",
				"apps[0] (Blog): name:",
				"apps[0] (Blog): source.repo:",
				"apps[0] (Blog): gitops.path: must be relative",
				`apps[0] (Blog): notify[0]: unknown notification "pager"`,
			},
		},
		{
			name:       "invalid file settings and apps reported together",
			args:       []string{"serve"},
			config:     "log:\n  format: xml\n" + invalidAppsConfig,
			wantStderr: []string{"doorstep: invalid configuration", "log format", "apps[0] (Blog): name:"},
		},
		{
			name:       "unknown key in file",
			args:       []string{"serve"},
			config:     "apps:\n  - appname: blog\n",
			wantStderr: []string{"doorstep: config file ", "line 2", "field appname not found"},
		},
		{
			name:       "missing config file",
			args:       []string{"serve", "--config", "/nonexistent/doorstep.yaml"},
			wantStderr: []string{"doorstep: config file /nonexistent/doorstep.yaml: no such file or directory"},
		},
		{
			name:       "empty config flag",
			args:       []string{"serve", "--config", ""},
			wantStderr: []string{"doorstep: --config: must not be empty"},
		},
		{
			name:       "empty config environment variable",
			args:       []string{"serve"},
			env:        map[string]string{config.EnvConfig: ""},
			wantStderr: []string{"doorstep: DOORSTEP_CONFIG: must not be empty"},
		},
		{
			name:       "unresolved secret",
			args:       []string{"serve", "--listen-addr", "127.0.0.1:0"},
			config:     oneAppConfig,
			env:        map[string]string{config.EnvDatabaseURL: "postgres://doorstep@localhost/doorstep"},
			wantStderr: []string{"doorstep: invalid configuration: notification ops webhook_url: environment variable TEST_SLACK_URL is not set"},
		},
		{
			name:       "empty secret is reported without its value",
			args:       []string{"serve", "--listen-addr", "127.0.0.1:0"},
			config:     oneAppConfig,
			env:        map[string]string{config.EnvDatabaseURL: "postgres://doorstep@localhost/doorstep", "TEST_SLACK_URL": "\n"},
			wantStderr: []string{"notification ops webhook_url: environment variable TEST_SLACK_URL is empty"},
		},
		{
			name:       "config validate without a file",
			args:       []string{"config", "validate"},
			wantStderr: []string{"doorstep: no configuration file: pass --config or set DOORSTEP_CONFIG"},
		},
		{
			name:       "config validate with invalid apps",
			args:       []string{"config", "validate"},
			config:     invalidAppsConfig,
			wantStderr: []string{"doorstep: invalid configuration", "apps[0] (Blog): source.repo:", "notify[0]"},
		},
		{
			name:       "config validate with invalid settings",
			args:       []string{"config", "validate"},
			config:     "server:\n  listen_addr: nowhere\n",
			wantStderr: []string{"doorstep: invalid configuration", "listen address"},
		},
		{
			name:       "config validate with unparseable file",
			args:       []string{"config", "validate"},
			config:     "apps: [\n",
			wantStderr: []string{"doorstep: config file ", "yaml:"},
		},
		{
			name:       "config validate rejects arguments",
			args:       []string{"config", "validate", "config.yaml"},
			wantStderr: []string{`unknown command "config.yaml"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := tt.args
			if tt.config != "" {
				args = append(slices.Clone(args), "--config", writeConfig(t, tt.config))
			}
			var stdout, stderr bytes.Buffer
			code := Run(t.Context(), args, &stdout, &stderr, envFrom(tt.env))
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
			for _, forbidden := range tt.forbidStderr {
				if strings.Contains(stderr.String(), forbidden) || strings.Contains(stdout.String(), forbidden) {
					t.Errorf("output leaks %q: stdout %q stderr %q", forbidden, stdout.String(), stderr.String())
				}
			}
			if strings.Contains(stderr.String(), "Usage:") {
				t.Errorf("stderr prints usage on error: %q", stderr.String())
			}
			if strings.Contains(stderr.String(), "supersecret") {
				t.Errorf("stderr leaks key material: %q", stderr.String())
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args []string
		want []string
	}{
		{
			args: []string{"serve", "--help"},
			want: []string{"--config", config.EnvConfig, "--listen-addr", "--log-level", "--log-format", "--shutdown-delay", "--shutdown-timeout", "--database-url-file", config.EnvListenAddr, config.EnvShutdownDelay, config.EnvDatabaseURL, config.EnvDatabaseURLFile},
		},
		{
			args: []string{"migrate", "--help"},
			want: []string{"status", "--config", "--log-level", "--log-format", "--database-url-file", config.EnvDatabaseURLFile},
		},
		{
			args: []string{"migrate", "status", "--help"},
			want: []string{"--config", "--database-url-file", "--log-level"},
		},
		{
			args: []string{"image", "resolve", "--help"},
			want: []string{"--config", "--registry-credentials-file", config.EnvRegistryCredentialsFile},
		},
		{
			args: []string{"argocd", "status", "--help"},
			want: []string{"--config", "--kubeconfig", "--kube-context", "--revision", config.EnvKubeconfig},
		},
		{
			args: []string{"argocd"},
			want: []string{"status"},
		},
		{
			args: []string{"config", "validate", "--help"},
			want: []string{"--config", config.EnvConfig, "without resolving secrets"},
		},
		{
			args: []string{"config"},
			want: []string{"validate"},
		},
		{
			args: []string{"--help"},
			want: []string{"config", "serve", "migrate", "version"},
		},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := Run(t.Context(), tt.args, &stdout, &stderr, envFrom(nil)); code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("help %q does not mention %q", stdout.String(), want)
				}
			}
		})
	}
}

func TestResolveConfigPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		changed   bool
		flagValue string
		env       map[string]string
		want      string
		wantErr   string
	}{
		{name: "none"},
		{name: "flag", changed: true, flagValue: "/etc/flag.yaml", want: "/etc/flag.yaml"},
		{name: "environment", env: map[string]string{config.EnvConfig: "/etc/env.yaml"}, want: "/etc/env.yaml"},
		{
			name:    "flag over environment",
			changed: true, flagValue: "/etc/flag.yaml",
			env:  map[string]string{config.EnvConfig: "/etc/env.yaml"},
			want: "/etc/flag.yaml",
		},
		{
			name:    "flag over empty environment",
			changed: true, flagValue: "/etc/flag.yaml",
			env:  map[string]string{config.EnvConfig: ""},
			want: "/etc/flag.yaml",
		},
		{name: "unchanged flag ignored", flagValue: "/etc/flag.yaml"},
		{name: "empty flag", changed: true, wantErr: "--config: must not be empty"},
		{name: "empty environment", env: map[string]string{config.EnvConfig: ""}, wantErr: "DOORSTEP_CONFIG: must not be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changed := func(name string) bool { return tt.changed && name == flagConfig }
			got, err := resolveConfigPath(changed, tt.flagValue, envFrom(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveConfigPath() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveConfigPath: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveConfigPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveServeConfig(t *testing.T) {
	t.Parallel()

	const databaseURL = "postgres://doorstep@localhost/doorstep"
	flags := config.Config{
		Server:     config.Server{ListenAddr: "127.0.0.1:7000", MetricsListenAddr: "127.0.0.1:7001", ShutdownDelay: time.Second, ShutdownTimeout: 3 * time.Second, PublicURL: "https://flag.example.com"},
		Log:        config.Log{Level: "error", Format: "text"},
		Database:   config.Database{URLFile: "/run/secrets/database-url"},
		GitHub:     config.Default().GitHub,
		Registry:   config.Registry{CredentialsFile: "/flag/config.json"},
		Locks:      config.Default().Locks,
		Kubernetes: config.Kubernetes{Kubeconfig: "/flag/kubeconfig", Context: "flag-context"},
	}
	const fileSettings = `
server:
  listen_addr: 127.0.0.1:6000
  metrics_listen_addr: ""
  shutdown_delay: 0s
  shutdown_timeout: 1m
log:
  level: warn
  format: text
database:
  url_file: /file/database-url
github:
  app_id: "42"
  private_key_file: /file/key.pem
  api_url: https://ghe.example.com/api/v3
registry:
  credentials_file: /file/config.json
locks:
  reminder_interval: 6h
kubernetes:
  kubeconfig: /file/kubeconfig
  context: file-context
`
	fileConfig := config.Config{
		Server:     config.Server{ListenAddr: "127.0.0.1:6000", ShutdownDelay: 0, ShutdownTimeout: time.Minute},
		Log:        config.Log{Level: "warn", Format: "text"},
		Database:   config.Database{URL: databaseURL},
		GitHub:     config.GitHub{AppID: "42", PrivateKeyFile: "/file/key.pem", APIURL: "https://ghe.example.com/api/v3"},
		Registry:   config.Registry{CredentialsFile: "/file/config.json"},
		Locks:      config.Locks{ReminderInterval: 6 * time.Hour},
		Kubernetes: config.Kubernetes{Kubeconfig: "/file/kubeconfig", Context: "file-context"},
	}
	oneAppCatalog := config.File{
		Notifications: []config.Notification{{Name: "ops", Type: config.NotifySlack, WebhookURLEnv: "TEST_SLACK_URL"}},
		Apps: []config.App{{
			Name:   "blog",
			Source: config.Source{Repo: "example-user/blog"},
			Image:  config.Image{Repository: "ghcr.io/example-user/blog"},
			GitOps: config.GitOps{Repo: "example-user/homelab", Path: "apps/blog"},
			Notify: []string{"ops"},
		}},
	}.Catalog()

	tests := []struct {
		name        string
		file        string
		env         map[string]string
		changed     []string
		want        func(*config.Config)
		wantCatalog config.Catalog
	}{
		{
			name: "defaults",
			want: func(*config.Config) {},
		},
		{
			name: "environment over defaults",
			env: map[string]string{
				config.EnvListenAddr:        ":9000",
				config.EnvMetricsListenAddr: ":9091",
				config.EnvShutdownDelay:     "0s",
				config.EnvShutdownTimeout:   "1s",
				config.EnvLogLevel:          "debug",
				config.EnvLogFormat:         "text",
			},
			want: func(c *config.Config) {
				c.Server = config.Server{ListenAddr: ":9000", MetricsListenAddr: ":9091", ShutdownDelay: 0, ShutdownTimeout: time.Second}
				c.Log = config.Log{Level: "debug", Format: "text"}
			},
		},
		{
			name: "metrics listen address from environment",
			env:  map[string]string{config.EnvMetricsListenAddr: "127.0.0.1:9100"},
			want: func(c *config.Config) { c.Server.MetricsListenAddr = "127.0.0.1:9100" },
		},
		{
			name:    "metrics listen address flag over environment",
			env:     map[string]string{config.EnvMetricsListenAddr: "127.0.0.1:9100"},
			changed: []string{flagMetricsListenAddr},
			want:    func(c *config.Config) { c.Server.MetricsListenAddr = flags.Server.MetricsListenAddr },
		},
		{
			name:    "registry credentials flag over environment",
			env:     map[string]string{config.EnvRegistryCredentialsFile: "/env/config.json"},
			changed: []string{flagRegistryAuthFile},
			want:    func(c *config.Config) { c.Registry.CredentialsFile = flags.Registry.CredentialsFile },
		},
		{
			name: "registry credentials file from environment",
			env:  map[string]string{config.EnvRegistryCredentialsFile: "/env/config.json"},
			want: func(c *config.Config) { c.Registry.CredentialsFile = "/env/config.json" },
		},
		{
			name:    "flags over defaults",
			changed: []string{flagListenAddr, flagMetricsListenAddr, flagShutdownDelay, flagShutdownTimeout, flagPublicURL, flagLogLevel, flagLogFormat, flagDatabaseURLFile, flagRegistryAuthFile, flagKubeconfig, flagKubeContext},
			want:    func(c *config.Config) { *c = flags },
		},
		{
			name: "public url from environment",
			env:  map[string]string{config.EnvPublicURL: "https://env.example.com"},
			want: func(c *config.Config) { c.Server.PublicURL = "https://env.example.com" },
		},
		{
			name:    "public url flag over environment",
			env:     map[string]string{config.EnvPublicURL: "https://env.example.com"},
			changed: []string{flagPublicURL},
			want:    func(c *config.Config) { c.Server.PublicURL = flags.Server.PublicURL },
		},
		{
			name: "database URL file from environment",
			env:  map[string]string{config.EnvDatabaseURL: "", config.EnvDatabaseURLFile: "/etc/doorstep/db"},
			want: func(c *config.Config) { c.Database = config.Database{URLFile: "/etc/doorstep/db"} },
		},
		{
			name:    "database URL file flag replaces the environment URL",
			changed: []string{flagDatabaseURLFile},
			want:    func(c *config.Config) { c.Database = flags.Database },
		},
		{
			name:    "database URL file flag replaces the environment file",
			env:     map[string]string{config.EnvDatabaseURL: "", config.EnvDatabaseURLFile: "/etc/doorstep/db"},
			changed: []string{flagDatabaseURLFile},
			want:    func(c *config.Config) { c.Database = flags.Database },
		},
		{
			name: "flags over environment",
			env: map[string]string{
				config.EnvListenAddr: ":9000",
				config.EnvLogLevel:   "debug",
			},
			changed: []string{flagListenAddr},
			want: func(c *config.Config) {
				c.Server.ListenAddr = flags.Server.ListenAddr
				c.Log.Level = "debug"
			},
		},
		{
			name:    "flag rescues invalid environment value",
			env:     map[string]string{config.EnvLogLevel: "loud"},
			changed: []string{flagLogLevel},
			want:    func(c *config.Config) { c.Log.Level = flags.Log.Level },
		},
		{
			name:    "unchanged flags ignored",
			env:     map[string]string{config.EnvLogFormat: "text"},
			changed: []string{flagShutdownTimeout},
			want: func(c *config.Config) {
				c.Server.ShutdownTimeout = flags.Server.ShutdownTimeout
				c.Log.Format = "text"
			},
		},
		{
			name: "file over defaults",
			file: fileSettings,
			want: func(c *config.Config) { *c = fileConfig },
		},
		{
			name: "partial file keeps defaults",
			file: "log:\n  level: debug\n",
			want: func(c *config.Config) { c.Log.Level = "debug" },
		},
		{
			name: "environment over file",
			file: fileSettings,
			env:  map[string]string{config.EnvListenAddr: ":9000", config.EnvShutdownDelay: "2s", config.EnvLockReminderInterval: "0s"},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Server.ListenAddr = ":9000"
				c.Server.ShutdownDelay = 2 * time.Second
				c.Locks.ReminderInterval = 0
			},
		},
		{
			name:    "flag over environment over file",
			file:    fileSettings,
			env:     map[string]string{config.EnvListenAddr: ":9000", config.EnvLogLevel: "debug"},
			changed: []string{flagListenAddr},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Server.ListenAddr = flags.Server.ListenAddr
				c.Log.Level = "debug"
			},
		},
		{
			name: "database URL file from the file without environment",
			file: fileSettings,
			env:  map[string]string{config.EnvDatabaseURL: ""},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Database = config.Database{URLFile: "/file/database-url"}
			},
		},
		{
			name: "database URL file environment over file",
			file: fileSettings,
			env:  map[string]string{config.EnvDatabaseURL: "", config.EnvDatabaseURLFile: "/env/database-url"},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Database = config.Database{URLFile: "/env/database-url"}
			},
		},
		{
			name:    "database URL file flag over environment over file",
			file:    fileSettings,
			changed: []string{flagDatabaseURLFile},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Database = flags.Database
			},
		},
		{
			name: "github and registry environment over file",
			file: fileSettings,
			env: map[string]string{
				config.EnvGitHubAppID:             "7",
				config.EnvGitHubAppPrivateKey:     "inline-key",
				config.EnvGitHubAPIURL:            "https://api.github.com",
				config.EnvRegistryCredentialsFile: "/env/config.json",
				config.EnvMetricsListenAddr:       "127.0.0.1:9100",
			},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Server.MetricsListenAddr = "127.0.0.1:9100"
				c.GitHub = config.GitHub{AppID: "7", PrivateKey: "inline-key", APIURL: "https://api.github.com"}
				c.Registry.CredentialsFile = "/env/config.json"
			},
		},
		{
			name:    "registry and metrics flags over file",
			file:    fileSettings,
			changed: []string{flagRegistryAuthFile, flagMetricsListenAddr},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Server.MetricsListenAddr = flags.Server.MetricsListenAddr
				c.Registry.CredentialsFile = flags.Registry.CredentialsFile
			},
		},
		{
			name: "kubernetes environment over file",
			file: fileSettings,
			env:  map[string]string{config.EnvKubeconfig: "/env/kubeconfig", config.EnvKubeContext: "env-context"},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Kubernetes = config.Kubernetes{Kubeconfig: "/env/kubeconfig", Context: "env-context"}
			},
		},
		{
			name:    "kubernetes flags over environment",
			file:    fileSettings,
			env:     map[string]string{config.EnvKubeconfig: "/env/kubeconfig"},
			changed: []string{flagKubeconfig, flagKubeContext},
			want: func(c *config.Config) {
				*c = fileConfig
				c.Kubernetes = flags.Kubernetes
			},
		},
		{
			name:    "environment rescues invalid file value",
			file:    "log:\n  format: xml\n",
			env:     map[string]string{config.EnvLogFormat: "json"},
			changed: nil,
			want:    func(*config.Config) {},
		},
		{
			name:        "apps come from the file",
			file:        oneAppConfig,
			want:        func(c *config.Config) { c.Log.Level = "debug" },
			wantCatalog: oneAppCatalog,
		},
		{
			name: "empty file",
			file: "\n",
			want: func(*config.Config) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changed := func(name string) bool { return slices.Contains(tt.changed, name) }
			env := map[string]string{config.EnvDatabaseURL: databaseURL}
			maps.Copy(env, tt.env)
			if env[config.EnvDatabaseURL] == "" {
				delete(env, config.EnvDatabaseURL)
			}
			want := config.Default()
			want.Database.URL = databaseURL
			tt.want(&want)
			var path string
			if tt.file != "" {
				path = writeConfig(t, tt.file)
			}

			got, catalog, err := resolveServeConfig(changed, flags, path, envFrom(env))
			if err != nil {
				t.Fatalf("resolveServeConfig: %v", err)
			}
			if got != want {
				t.Errorf("resolveServeConfig() = %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(catalog, tt.wantCatalog) {
				t.Errorf("resolveServeConfig() catalog = %+v, want %+v", catalog, tt.wantCatalog)
			}
		})
	}
}

func TestResolveServeConfigErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		path    string
		env     map[string]string
		wantErr []string
	}{
		{
			name:    "missing file",
			path:    "/nonexistent/doorstep.yaml",
			wantErr: []string{"config file /nonexistent/doorstep.yaml: no such file or directory"},
		},
		{
			name:    "unknown field",
			file:    "server:\n  listen: :80\n",
			wantErr: []string{"config file ", "line 2", "field listen not found"},
		},
		{
			name:    "invalid environment over file",
			file:    "log:\n  level: debug\n",
			env:     map[string]string{config.EnvShutdownDelay: "later"},
			wantErr: []string{"invalid environment", config.EnvShutdownDelay},
		},
		{
			name:    "invalid file value",
			file:    "server:\n  shutdown_timeout: 0s\n",
			wantErr: []string{"invalid configuration", "shutdown timeout: must be positive"},
		},
		{
			name:    "invalid settings and apps together",
			file:    "log:\n  level: loud\n" + invalidAppsConfig,
			wantErr: []string{"invalid configuration", "log level", "apps[0] (Blog): gitops.path"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := tt.path
			if tt.file != "" {
				path = writeConfig(t, tt.file)
			}
			_, _, err := resolveServeConfig(func(string) bool { return false }, config.Config{}, path, envFrom(tt.env))
			if err == nil {
				t.Fatal("resolveServeConfig() = nil error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestRunConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		useEnv  bool
		wantOut string
	}{
		{name: "one app by flag", file: oneAppConfig, wantOut: "ok: 1 apps, 1 notification targets\n"},
		{name: "one app by environment", file: oneAppConfig, useEnv: true, wantOut: "ok: 1 apps, 1 notification targets\n"},
		{name: "empty file", file: "\n", wantOut: "ok: 0 apps, 0 notification targets\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeConfig(t, tt.file)
			args := []string{"config", "validate", "--config", path}
			env := map[string]string{}
			if tt.useEnv {
				args = []string{"config", "validate"}
				env[config.EnvConfig] = path
			}
			var stdout, stderr bytes.Buffer
			if code := Run(t.Context(), args, &stdout, &stderr, envFrom(env)); code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
			}
			if stdout.String() != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantOut)
			}
			if stderr.Len() != 0 {
				t.Errorf("unexpected stderr %q", stderr.String())
			}
		})
	}
}

func TestRunServe(t *testing.T) {
	t.Parallel()

	keyPEM := string(testKeyPEM(t))
	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		database databaseSource
		config   string
		wantLogs []string
		wantApps float64

		wantCredentialEntries float64
	}{
		{
			name:     "flags",
			args:     []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--log-level", "debug", "--shutdown-delay", "0s"},
			database: databaseFileFlag,
			wantLogs: []string{"github app not configured"},
		},
		{
			name:                  "registry credentials from flag",
			args:                  []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--log-level", "debug", "--shutdown-delay", "0s", "--registry-credentials-file", "testdata/dockerconfig.json"},
			database:              databaseFileFlag,
			wantCredentialEntries: 2,
		},
		{
			name:                  "registry credentials from environment",
			args:                  []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--log-level", "debug", "--shutdown-delay", "0s"},
			env:                   map[string]string{config.EnvRegistryCredentialsFile: "testdata/dockerconfig.json"},
			wantCredentialEntries: 2,
		},
		{
			name:     "database URL file from environment",
			args:     []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s", "--log-level", "debug"},
			database: databaseFileEnv,
		},
		{
			name: "environment",
			args: []string{"serve"},
			env:  map[string]string{config.EnvListenAddr: "127.0.0.1:0", config.EnvMetricsListenAddr: "127.0.0.1:0", config.EnvLogLevel: "debug", config.EnvShutdownDelay: "0s"},
		},
		{
			name: "flag beats environment",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--log-format", "json"},
			env: map[string]string{
				config.EnvShutdownDelay: "10ms",
				config.EnvListenAddr:    "invalid",
				config.EnvLogFormat:     "text",
				config.EnvLogLevel:      "debug",
			},
		},
		{
			name: "github app configured",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s"},
			env: map[string]string{
				config.EnvLogLevel:            "debug",
				config.EnvGitHubAppID:         "123456",
				config.EnvGitHubAppPrivateKey: keyPEM,
			},
			wantLogs: []string{"github app configured"},
		},
		{
			name:     "configuration file",
			args:     []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s"},
			env:      map[string]string{"TEST_SLACK_URL": "https://hooks.slack.com/services/T/B/x\n"},
			config:   oneAppConfig,
			wantApps: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr lockedBuffer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			args, env := withDatabase(t, tt.database, tt.args, tt.env)
			var path string
			if tt.config != "" {
				path = writeConfig(t, tt.config)
				args = append(args, "--config", path)
			}
			done := make(chan int, 1)
			go func() { done <- Run(ctx, args, &stdout, &stderr, envFrom(env)) }()

			addr := waitForListening(t, &stderr, done)
			if code, body := get(t, "http://"+addr+"/healthz"); code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
				t.Errorf("GET /healthz = %d %q", code, body)
			}
			if code, body := get(t, "http://"+addr+"/readyz"); code != http.StatusOK || !strings.Contains(body, `"postgres":"ok"`) {
				t.Errorf("GET /readyz = %d %q", code, body)
			}
			internalAddr := waitForMetricsAddr(t, &stderr, done)
			if code, body := get(t, "http://"+addr+"/api/v1/apps/blog/queue"); code != http.StatusUnauthorized || !strings.Contains(body, "unauthenticated") {
				t.Errorf("GET /api/v1/apps/blog/queue without a token = %d %q, want 401", code, body)
			}
			if code, body := get(t, "http://"+internalAddr+"/api/v1/apps/blog/queue"); code != http.StatusNotFound || strings.Contains(body, `"code":"not_found"`) {
				t.Errorf("metrics listener serves the API: GET /api/v1/apps/blog/queue = %d %q", code, body)
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
			if strings.Contains(stderr.String(), "PRIVATE KEY") {
				t.Errorf("logs leak the private key: %s", stderr.String())
			}
			assertCredentialsLog(t, logs, tt.wantCredentialEntries)
			if strings.Contains(stderr.String(), "s3cret") {
				t.Errorf("logs leak registry credentials: %s", stderr.String())
			}
			for _, msg := range append([]string{"database ready", "listening", "request", "shutting down", "stopped"}, tt.wantLogs...) {
				if !hasMessage(logs, msg) {
					t.Errorf("logs missing %q: %s", msg, stderr.String())
				}
			}
			assertConfigLogs(t, logs, path, tt.wantApps)
			if stdout.Len() != 0 {
				t.Errorf("unexpected stdout %q", stdout.String())
			}
		})
	}
}

func TestRunServeListenFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "main listener", args: []string{"serve", "--listen-addr", "256.0.0.1:0"}},
		{name: "metrics listener", args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "256.0.0.1:0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			env := map[string]string{config.EnvDatabaseURL: storetest.DatabaseURL(t)}
			code := Run(t.Context(), tt.args, &stdout, &stderr, envFrom(env))
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "doorstep: listen on 256.0.0.1:0") {
				t.Errorf("stderr %q does not report the listen failure", stderr.String())
			}
		})
	}
}

func TestRunServeMetrics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		args         []string
		env          map[string]string
		wantSeparate bool
	}{
		{
			name: "main listener when the metrics address is empty",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "", "--shutdown-delay", "0s"},
		},
		{
			name:         "separate listener from flag",
			args:         []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s"},
			wantSeparate: true,
		},
		{
			name: "separate listener from environment",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s"},
			env:  map[string]string{config.EnvMetricsListenAddr: "127.0.0.1:0"},

			wantSeparate: true,
		},
		{
			name:         "flag beats environment",
			args:         []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s"},
			env:          map[string]string{config.EnvMetricsListenAddr: "invalid"},
			wantSeparate: true,
		},
		{
			name: "empty flag beats environment",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "", "--shutdown-delay", "0s"},
			env:  map[string]string{config.EnvMetricsListenAddr: "127.0.0.1:0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr lockedBuffer
			args, env := withDatabase(t, databaseURLEnv, tt.args, tt.env)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan int, 1)
			go func() { done <- Run(ctx, args, &stdout, &stderr, envFrom(env)) }()

			mainAddr := waitForListening(t, &stderr, done)
			metricsAddr := mainAddr
			if tt.wantSeparate {
				metricsAddr = waitForMetricsAddr(t, &stderr, done)
				if metricsAddr == mainAddr {
					t.Fatalf("metrics address equals main address %s", mainAddr)
				}
				code, _ := get(t, "http://"+mainAddr+"/metrics")
				if code != http.StatusNotFound {
					t.Errorf("main GET /metrics = %d, want %d", code, http.StatusNotFound)
				}
			}
			if code, body := get(t, "http://"+mainAddr+"/api/v1/locks"); code != http.StatusUnauthorized || !strings.Contains(body, "unauthenticated") {
				t.Errorf("main GET /api/v1/locks = %d %q, want 401", code, body)
			}
			if code, body := get(t, "http://"+metricsAddr+"/api/v1/locks"); tt.wantSeparate && (code != http.StatusNotFound || strings.Contains(body, `"code":"not_found"`)) {
				t.Errorf("metrics GET /api/v1/locks = %d %q, want the API unmounted", code, body)
			}

			code, body := get(t, "http://"+metricsAddr+"/metrics")
			if code != http.StatusOK || !strings.Contains(body, "doorstep_build_info{") {
				t.Errorf("GET /metrics = %d %.200q", code, body)
			}
			if !strings.Contains(body, "\ndoorstep_queue_depth 0\n") {
				t.Errorf("GET /metrics does not report an empty queue: %.2000q", body)
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
			if !hasMessage(parseLogs(t, stderr.String()), "stopped") {
				t.Errorf("logs missing stopped: %s", stderr.String())
			}
		})
	}
}

func TestRunServers(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	waitForCancel := func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}
	tests := []struct {
		name    string
		runs    []func(context.Context) error
		wantErr error
	}{
		{name: "no servers", wantErr: nil},
		{
			name:    "one failure stops the other",
			runs:    []func(context.Context) error{func(context.Context) error { return errBoom }, waitForCancel},
			wantErr: errBoom,
		},
		{
			name:    "clean exit stops the other",
			runs:    []func(context.Context) error{func(context.Context) error { return nil }, waitForCancel},
			wantErr: nil,
		},
		{
			name: "all errors returned",
			runs: []func(context.Context) error{
				func(context.Context) error { return errBoom },
				func(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) },
			},
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			done := make(chan error, 1)
			go func() { done <- runServers(t.Context(), tt.runs...) }()
			select {
			case err := <-done:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("runServers() = %v, want %v", err, tt.wantErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runServers did not return")
			}
		})
	}
}

func TestRunServersParentCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{}, 2)
	run := func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- runServers(ctx, run, run) }()
	<-started
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runServers() = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServers did not return after cancellation")
	}
}

func TestRunMigrate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		database databaseSource
	}{
		{name: "environment URL", database: databaseURLEnv},
		{name: "file flag", database: databaseFileFlag},
		{name: "file environment", database: databaseFileEnv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			databaseArgs, env := withDatabase(t, tt.database, nil, nil)
			run := func(args ...string) string {
				t.Helper()
				args = append(args, databaseArgs...)
				var stdout, stderr bytes.Buffer
				if code := Run(t.Context(), args, &stdout, &stderr, envFrom(env)); code != 0 {
					t.Fatalf("%v: exit code = %d, stderr %q", args, code, stderr.String())
				}
				return stdout.String()
			}

			versions := migrationVersionsOf(t)
			header := `^VERSION\s+STATE\s+APPLIED AT\s+NAME$`
			pending := []string{header}
			applied := []string{}
			done := []string{header}
			for _, v := range versions {
				pending = append(pending, fmt.Sprintf(`^%d\s+pending\s+-\s+%s$`, v.version, regexp.QuoteMeta(v.name)))
				applied = append(applied, fmt.Sprintf(`^applied %d %s \(\S+\)$`, v.version, regexp.QuoteMeta(v.name)))
				done = append(done, fmt.Sprintf(`^%d\s+applied\s+\d{4}-\d{2}-\d{2}T\S+Z\s+%s$`, v.version, regexp.QuoteMeta(v.name)))
			}

			assertLines(t, run("migrate", "status"), pending...)
			assertLines(t, run("migrate"), applied...)
			assertLines(t, run("migrate"), `^no pending migrations$`)
			assertLines(t, run("migrate", "status", "--log-format", "text"), done...)
		})
	}
}

func TestRunMigrateThenServe(t *testing.T) {
	t.Parallel()

	args, env := withDatabase(t, databaseURLEnv, []string{"migrate"}, nil)
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), args, &stdout, &stderr, envFrom(env)); code != 0 {
		t.Fatalf("migrate exit code = %d, stderr %q", code, stderr.String())
	}

	var logs lockedBuffer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"serve", "--listen-addr", "127.0.0.1:0", "--metrics-listen-addr", "127.0.0.1:0", "--shutdown-delay", "0s"}, io.Discard, &logs, envFrom(env))
	}()
	waitForListening(t, &logs, done)
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("serve exit code = %d, stderr %q", code, logs.String())
	}
	for _, entry := range parseLogs(t, logs.String()) {
		if entry["msg"] != "database ready" {
			continue
		}
		if applied, ok := entry["applied_migrations"].([]any); !ok || len(applied) != 0 {
			t.Errorf("serve applied migrations %v after migrate, want none", entry["applied_migrations"])
		}
		return
	}
	t.Errorf("serve never logged database ready: %s", logs.String())
}

type databaseSource int

const (
	databaseURLEnv databaseSource = iota
	databaseFileFlag
	databaseFileEnv
)

func withDatabase(t *testing.T, source databaseSource, args []string, env map[string]string) ([]string, map[string]string) {
	t.Helper()
	databaseURL := storetest.DatabaseURL(t)
	merged := maps.Clone(env)
	if merged == nil {
		merged = map[string]string{}
	}
	args = slices.Clone(args)
	switch source {
	case databaseURLEnv:
		merged[config.EnvDatabaseURL] = databaseURL
	case databaseFileFlag:
		args = append(args, "--database-url-file", writeDatabaseURLFile(t, databaseURL))
	case databaseFileEnv:
		merged[config.EnvDatabaseURLFile] = writeDatabaseURLFile(t, databaseURL)
	}
	return args, merged
}

func writeDatabaseURLFile(t *testing.T, databaseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(path, []byte(databaseURL+"\n"), 0o600); err != nil {
		t.Fatalf("write database URL file: %v", err)
	}
	return path
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

type migrationFile struct {
	version int
	name    string
}

func migrationVersionsOf(t *testing.T) []migrationFile {
	t.Helper()
	entries, err := os.ReadDir("../store/migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var files []migrationFile
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("migration %s has no numeric version: %v", e.Name(), err)
		}
		files = append(files, migrationFile{version: version, name: e.Name()})
	}
	if len(files) == 0 {
		t.Fatal("found no migrations")
	}
	return files
}

func assertLines(t *testing.T, out string, patterns ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(patterns) {
		t.Fatalf("output has %d lines, want %d: %q", len(lines), len(patterns), out)
	}
	for i, pattern := range patterns {
		if !regexp.MustCompile(pattern).MatchString(lines[i]) {
			t.Errorf("line %d %q does not match %s", i, lines[i], pattern)
		}
	}
}

func assertCredentialsLog(t *testing.T, logs []map[string]any, wantEntries float64) {
	t.Helper()
	for _, entry := range logs {
		if entry["msg"] != "registry credentials loaded" {
			continue
		}
		if wantEntries == 0 {
			t.Errorf("unexpected credentials log %v", entry)
		}
		if entry["entries"] != wantEntries {
			t.Errorf("credentials log entries = %v, want %v", entry["entries"], wantEntries)
		}
		return
	}
	if wantEntries != 0 {
		t.Errorf("logs missing %q", "registry credentials loaded")
	}
}

func assertConfigLogs(t *testing.T, logs []map[string]any, path string, wantApps float64) {
	t.Helper()
	loaded := findMessage(logs, "configuration loaded")
	switch {
	case path == "" && loaded != nil:
		t.Errorf("configuration loaded logged without a file: %v", loaded)
	case path != "" && loaded == nil:
		t.Errorf("logs missing configuration loaded")
	case path != "":
		if loaded["path"] != path || loaded["apps"] != wantApps || loaded["notifications"] != float64(1) {
			t.Errorf("configuration loaded = %v, want path %s and %v apps", loaded, path, wantApps)
		}
	}
	if warned := findMessage(logs, "no apps configured") != nil; warned != (wantApps == 0) {
		t.Errorf("no apps configured warning logged = %t, want %t", warned, wantApps == 0)
	}
}

func findMessage(entries []map[string]any, msg string) map[string]any {
	for _, entry := range entries {
		if entry["msg"] == msg {
			return entry
		}
	}
	return nil
}

func waitForListening(t *testing.T, stderr *lockedBuffer, done <-chan int) string {
	t.Helper()
	entry := waitForLog(t, stderr, done, "listening")
	addr, ok := entry["addr"].(string)
	if !ok {
		t.Fatalf("listening log has no addr: %v", entry)
	}
	build, ok := entry["build"].(map[string]any)
	if !ok || build["version"] == "" {
		t.Errorf("listening log has no build info: %v", entry)
	}
	return addr
}

func waitForLog(t *testing.T, stderr *lockedBuffer, done <-chan int, msg string) map[string]any {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if entry := findMessage(parseLogs(t, stderr.String()), msg); entry != nil {
			return entry
		}
		select {
		case code := <-done:
			t.Fatalf("serve exited with %d before logging %q: %s", code, msg, stderr.String())
		case <-t.Context().Done():
			t.Fatalf("test ended before serve logged %q: %s", msg, stderr.String())
		case <-ticker.C:
		}
	}
}

func parseLogs(t *testing.T, out string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", scanner.Text(), err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func hasMessage(entries []map[string]any, msg string) bool {
	return findMessage(entries, msg) != nil
}

func waitForMetricsAddr(t *testing.T, stderr *lockedBuffer, done <-chan int) string {
	t.Helper()
	entry := waitForLog(t, stderr, done, "metrics listening")
	addr, ok := entry["addr"].(string)
	if !ok {
		t.Fatalf("metrics listening log has no addr: %v", entry)
	}
	return addr
}

func TestRunServeMetricsBindFailureReleasesMainListener(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	occupied, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	mainProbe, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	mainAddr := mainProbe.Addr().String()
	if err := mainProbe.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var stdout, stderr bytes.Buffer
	args, env := withDatabase(t, databaseURLEnv, []string{"serve", "--listen-addr", mainAddr, "--metrics-listen-addr", occupied.Addr().String()}, nil)
	if code := Run(t.Context(), args, &stdout, &stderr, envFrom(env)); code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "listen on "+occupied.Addr().String()) {
		t.Errorf("stderr %q does not name the metrics address", stderr.String())
	}

	reused, err := lc.Listen(t.Context(), "tcp", mainAddr)
	if err != nil {
		t.Fatalf("main listener was not released: %v", err)
	}
	_ = reused.Close()
}
