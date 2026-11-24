package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const fullFile = `
server:
  listen_addr: 127.0.0.1:9000
  metrics_listen_addr: 127.0.0.1:9001
  shutdown_delay: 0s
  shutdown_timeout: 1m
  public_url: https://doorstep.example.com
log:
  level: debug
  format: text
database:
  url_file: /etc/doorstep/secrets/database-url
github:
  app_id: "123"
  private_key_file: /etc/doorstep/secrets/github-app.pem
  api_url: https://ghe.example.com/api/v3
  webhook_secret_file: /etc/doorstep/secrets/github-webhook-secret
registry:
  credentials_file: /etc/doorstep/registry/.dockerconfigjson
locks:
  reminder_interval: 12h
kubernetes:
  kubeconfig: /etc/doorstep/kube/config
  context: homelab
intake:
  poll_interval: 2m
engine:
  concurrency: 8
notifications:
  - name: ops
    type: slack
    webhook_url_env: SLACK_WEBHOOK_URL
  - name: hook
    type: webhook
    url: https://hooks.example.com/doorstep
    secret_file: /etc/doorstep/secrets/hook-secret
    events: [deploy.failed, deploy.rolled_back]
apps:
  - name: blog
    source:
      repo: example-user/blog
      branch: trunk
      required_checks: [test, lint]
      deployments: false
      statuses: false
      status_messages: true
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-org/homelab
      branch: deploy
      path: apps/blog/
    pin:
      yaml:
        file: values.yaml
        path: image.digest
    deploy:
      mode: pull_request
      max_batch: 5
    argocd:
      application: blog-prod
      namespace: gitops
      sync_timeout: 15m
    gates:
      before:
        - type: checks
          timeout: 45m
      after:
        - type: health
          name: argo-health
        - type: soak
          duration: 15m
        - type: external
          name: e2e
          reporters: [token:ci-e2e]
    notify: [ops, hook]
`

func strPtr(s string) *string { return &s }

func intPtr(n int) *int { return &n }

func durPtr(d time.Duration) *Duration {
	v := Duration(d)
	return &v
}

func TestParse(t *testing.T) {
	t.Parallel()

	want := File{
		Server: FileServer{
			ListenAddr:        strPtr("127.0.0.1:9000"),
			MetricsListenAddr: strPtr("127.0.0.1:9001"),
			ShutdownDelay:     durPtr(0),
			ShutdownTimeout:   durPtr(time.Minute),
			PublicURL:         strPtr("https://doorstep.example.com"),
		},
		Log:      FileLog{Level: strPtr("debug"), Format: strPtr("text")},
		Database: FileDatabase{URLFile: strPtr("/etc/doorstep/secrets/database-url")},
		GitHub: FileGitHub{
			AppID:             strPtr("123"),
			PrivateKeyFile:    strPtr("/etc/doorstep/secrets/github-app.pem"),
			APIURL:            strPtr("https://ghe.example.com/api/v3"),
			WebhookSecretFile: strPtr("/etc/doorstep/secrets/github-webhook-secret"),
		},
		Registry:   FileRegistry{CredentialsFile: strPtr("/etc/doorstep/registry/.dockerconfigjson")},
		Locks:      FileLocks{ReminderInterval: durPtr(12 * time.Hour)},
		Intake:     FileIntake{PollInterval: durPtr(2 * time.Minute)},
		Engine:     FileEngine{Concurrency: intPtr(8)},
		Kubernetes: FileKubernetes{Kubeconfig: strPtr("/etc/doorstep/kube/config"), Context: strPtr("homelab")},
		Notifications: []Notification{
			{Name: "ops", Type: NotifySlack, WebhookURLEnv: "SLACK_WEBHOOK_URL"},
			{
				Name: "hook", Type: NotifyWebhook, URL: "https://hooks.example.com/doorstep", SecretFile: "/etc/doorstep/secrets/hook-secret",
				Events: []string{"deploy.failed", "deploy.rolled_back"},
			},
		},
		Apps: []App{{
			Name:   "blog",
			Source: Source{Repo: "example-user/blog", Branch: "trunk", RequiredChecks: []string{"test", "lint"}, Deployments: new(false), Statuses: new(false), StatusMessages: true},
			Image:  Image{Repository: "ghcr.io/example-user/blog"},
			GitOps: GitOps{Repo: "example-org/homelab", Branch: "deploy", Path: "apps/blog/"},
			Pin:    Pin{YAML: &YAMLPin{File: "values.yaml", Path: "image.digest"}},
			Deploy: Deploy{Mode: DeployPullRequest, MaxBatch: intPtr(5)},
			ArgoCD: ArgoCD{Application: "blog-prod", Namespace: "gitops", SyncTimeout: durPtr(15 * time.Minute)},
			Gates: Gates{
				Before: []Gate{{Type: GateChecks, Timeout: durPtr(45 * time.Minute)}},
				After: []Gate{
					{Type: GateHealth, Name: "argo-health"},
					{Type: GateSoak, Duration: Duration(15 * time.Minute)},
					{Type: GateExternal, Name: "e2e", Reporters: []string{"token:ci-e2e"}},
				},
			},
			Notify: []string{"ops", "hook"},
		}},
	}

	got, err := Parse(strings.NewReader(fullFile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Parse() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseEmpty(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "\n\n", "# only a comment\n", "---\n", "--- {}\n"} {
		got, err := Parse(strings.NewReader(input))
		if err != nil {
			t.Errorf("Parse(%q): %v", input, err)
		}
		if !reflect.DeepEqual(got, File{}) {
			t.Errorf("Parse(%q) = %+v, want empty", input, got)
		}
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr []string
	}{
		{
			name:    "unknown top-level key",
			input:   "apps: []\napp: []\n",
			wantErr: []string{"line 2", "field app not found"},
		},
		{
			name:    "unknown server key",
			input:   "server:\n  listen_address: :80\n",
			wantErr: []string{"line 2", "field listen_address not found"},
		},
		{
			name:    "unknown app key",
			input:   "apps:\n  - name: blog\n    sorce:\n      repo: a/b\n",
			wantErr: []string{"line 3", "field sorce not found"},
		},
		{
			name:    "unknown nested app key",
			input:   "apps:\n  - name: blog\n    pin:\n      kustomize:\n        images: x\n",
			wantErr: []string{"line 5", "field images not found"},
		},
		{
			name:    "unknown gate key",
			input:   "apps:\n  - name: blog\n    gates:\n      after:\n        - type: soak\n          duraton: 5m\n",
			wantErr: []string{"line 6", "field duraton not found"},
		},
		{
			name:    "unknown job gate key",
			input:   "apps:\n  - name: blog\n    gates:\n      after:\n        - type: job\n          job:\n            namespace: blog\n            logs: x\n",
			wantErr: []string{"line 8", "field logs not found"},
		},
		{
			name:    "unknown notification key",
			input:   "notifications:\n  - name: ops\n    type: slack\n    webhook_url: https://hooks.slack.com/x\n",
			wantErr: []string{"line 4", "field webhook_url not found"},
		},
		{
			name:    "inline database URL",
			input:   "database:\n  url: postgres://doorstep:secret@db/doorstep\n",
			wantErr: []string{"line 2", "field url not found"},
		},
		{
			name:    "inline github private key",
			input:   "github:\n  private_key: x\n",
			wantErr: []string{"line 2", "field private_key not found"},
		},
		{
			name:    "every unknown key is reported",
			input:   "lgo: {}\napps:\n  - appname: blog\n",
			wantErr: []string{"field lgo not found", "field appname not found"},
		},
		{
			name:    "bad duration",
			input:   "server:\n  shutdown_timeout: 30\n",
			wantErr: []string{"line 2", "with a unit"},
		},
		{
			name:    "wrong type",
			input:   "apps: blog\n",
			wantErr: []string{"line 1", "cannot unmarshal"},
		},
		{
			name:    "syntax error",
			input:   "apps:\n  - name: [blog\n",
			wantErr: []string{"yaml:"},
		},
		{
			name:    "second document",
			input:   "apps: []\n---\napps: []\n",
			wantErr: []string{"multiple YAML documents are not supported"},
		},
		{
			name:    "empty second document",
			input:   "apps: []\n---\n",
			wantErr: []string{"multiple YAML documents are not supported"},
		},
		{
			name:    "broken second document",
			input:   "apps: []\n---\n[\n",
			wantErr: []string{"yaml:"},
		},
		{
			name:    "oversize input",
			input:   "# " + strings.Repeat("x", maxFileBytes) + "\n",
			wantErr: []string{"larger than the 1048576 byte limit"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(strings.NewReader(tt.input))
			assertErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestParseAtSizeLimit(t *testing.T) {
	t.Parallel()

	input := "# " + strings.Repeat("x", maxFileBytes-3) + "\n"
	if len(input) != maxFileBytes {
		t.Fatalf("input is %d bytes, want %d", len(input), maxFileBytes)
	}
	if _, err := Parse(strings.NewReader(input)); err != nil {
		t.Errorf("Parse at the limit: %v", err)
	}
}

func TestParseReadError(t *testing.T) {
	t.Parallel()

	readErr := errors.New("disk on fire")
	_, err := Parse(iotest.ErrReader(readErr))
	if !errors.Is(err, readErr) {
		t.Errorf("Parse() error = %v, want %v", err, readErr)
	}
}

func TestFileApply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file File
		base func(*Config)
		want func(*Config)
	}{
		{name: "empty file keeps base", file: File{}, want: func(*Config) {}},
		{
			name: "listen address",
			file: File{Server: FileServer{ListenAddr: strPtr(":9000")}},
			want: func(c *Config) { c.Server.ListenAddr = ":9000" },
		},
		{
			name: "zero shutdown delay is applied",
			file: File{Server: FileServer{ShutdownDelay: durPtr(0)}},
			want: func(c *Config) { c.Server.ShutdownDelay = 0 },
		},
		{
			name: "shutdown timeout",
			file: File{Server: FileServer{ShutdownTimeout: durPtr(time.Minute)}},
			want: func(c *Config) { c.Server.ShutdownTimeout = time.Minute },
		},
		{
			name: "empty log level is applied and left to validation",
			file: File{Log: FileLog{Level: strPtr("")}},
			want: func(c *Config) { c.Log.Level = "" },
		},
		{
			name: "log format",
			file: File{Log: FileLog{Format: strPtr("text")}},
			want: func(c *Config) { c.Log.Format = "text" },
		},
		{
			name: "empty metrics listen address is applied",
			file: File{Server: FileServer{MetricsListenAddr: strPtr("")}},
			want: func(c *Config) { c.Server.MetricsListenAddr = "" },
		},
		{
			name: "public url",
			file: File{Server: FileServer{PublicURL: strPtr("https://doorstep.example.com")}},
			want: func(c *Config) { c.Server.PublicURL = "https://doorstep.example.com" },
		},
		{
			name: "database URL file replaces an inline URL",
			file: File{Database: FileDatabase{URLFile: strPtr("/secrets/db")}},
			base: func(c *Config) { c.Database.URL = "postgres://localhost/doorstep" },
			want: func(c *Config) { c.Database = Database{URLFile: "/secrets/db"} },
		},
		{
			name: "github private key file replaces an inline key",
			file: File{GitHub: FileGitHub{PrivateKeyFile: strPtr("/secrets/key.pem")}},
			base: func(c *Config) { c.GitHub.PrivateKey = "inline" },
			want: func(c *Config) { c.GitHub.PrivateKey, c.GitHub.PrivateKeyFile = "", "/secrets/key.pem" },
		},
		{
			name: "github webhook secret file replaces an inline secret",
			file: File{GitHub: FileGitHub{WebhookSecretFile: strPtr("/secrets/webhook")}},
			base: func(c *Config) { c.GitHub.WebhookSecret = "inline" },
			want: func(c *Config) { c.GitHub.WebhookSecret, c.GitHub.WebhookSecretFile = "", "/secrets/webhook" },
		},
		{
			name: "zero poll interval is applied",
			file: File{Intake: FileIntake{PollInterval: durPtr(0)}},
			want: func(c *Config) { c.Intake.PollInterval = 0 },
		},
		{
			name: "zero lock reminder interval is applied",
			file: File{Locks: FileLocks{ReminderInterval: durPtr(0)}},
			want: func(c *Config) { c.Locks.ReminderInterval = 0 },
		},
		{
			name: "all settings",
			file: File{
				Server:     FileServer{ListenAddr: strPtr("127.0.0.1:1"), MetricsListenAddr: strPtr("127.0.0.1:2"), ShutdownDelay: durPtr(time.Second), ShutdownTimeout: durPtr(2 * time.Second), PublicURL: strPtr("https://deploy.example.com")},
				Log:        FileLog{Level: strPtr("warn"), Format: strPtr("text")},
				Database:   FileDatabase{URLFile: strPtr("/secrets/db")},
				GitHub:     FileGitHub{AppID: strPtr("42"), PrivateKeyFile: strPtr("/secrets/key.pem"), APIURL: strPtr("https://ghe.example.com/api/v3"), WebhookSecretFile: strPtr("/secrets/webhook")},
				Registry:   FileRegistry{CredentialsFile: strPtr("/secrets/config.json")},
				Locks:      FileLocks{ReminderInterval: durPtr(6 * time.Hour)},
				Intake:     FileIntake{PollInterval: durPtr(30 * time.Second)},
				Engine:     FileEngine{Concurrency: intPtr(12)},
				Kubernetes: FileKubernetes{Kubeconfig: strPtr("/kube/config"), Context: strPtr("lab")},
			},
			want: func(c *Config) {
				c.Server = Server{ListenAddr: "127.0.0.1:1", MetricsListenAddr: "127.0.0.1:2", ShutdownDelay: time.Second, ShutdownTimeout: 2 * time.Second, PublicURL: "https://deploy.example.com"}
				c.Log = Log{Level: "warn", Format: "text"}
				c.Database = Database{URLFile: "/secrets/db"}
				c.GitHub = GitHub{AppID: "42", PrivateKeyFile: "/secrets/key.pem", APIURL: "https://ghe.example.com/api/v3", WebhookSecretFile: "/secrets/webhook"}
				c.Registry = Registry{CredentialsFile: "/secrets/config.json"}
				c.Locks = Locks{ReminderInterval: 6 * time.Hour}
				c.Kubernetes = Kubernetes{Kubeconfig: "/kube/config", Context: "lab"}
				c.Intake = Intake{PollInterval: 30 * time.Second}
				c.Engine = Engine{Concurrency: 12}
			},
		},
		{
			name: "public url",
			file: File{Server: FileServer{PublicURL: strPtr("https://deploy.example.com")}},
			want: func(c *Config) { c.Server.PublicURL = "https://deploy.example.com" },
		},
		{
			name: "auth does not change runtime settings",
			file: File{Auth: Auth{GitHub: &GitHubLogin{ClientID: "x"}}},
			want: func(*Config) {},
		},
		{
			name: "apps do not change runtime settings",
			file: File{Apps: []App{{Name: "blog"}}},
			want: func(*Config) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := Default()
			if tt.base != nil {
				tt.base(&base)
			}
			want := base
			tt.want(&want)
			if got := tt.file.Apply(base); got != want {
				t.Errorf("Apply() = %+v, want %+v", got, want)
			}
		})
	}
}

func minimalApp() App {
	return App{
		Name:   "blog",
		Source: Source{Repo: "example-user/blog"},
		Image:  Image{Repository: "ghcr.io/example-user/blog"},
		GitOps: GitOps{Repo: "example-user/homelab", Path: "apps/blog"},
	}
}

func defaultedApp() App {
	return App{
		Name:   "blog",
		Source: Source{Repo: "example-user/blog", Branch: "main"},
		Image:  Image{Repository: "ghcr.io/example-user/blog"},
		GitOps: GitOps{Repo: "example-user/homelab", Branch: "main", Path: "apps/blog"},
		Pin:    Pin{Kustomize: &KustomizePin{Image: "ghcr.io/example-user/blog"}},
		Deploy: Deploy{Mode: DeployCommit, MaxBatch: intPtr(DefaultMaxBatch)},
		ArgoCD: ArgoCD{Application: "blog", Namespace: "argocd", SyncTimeout: durPtr(core.DefaultSyncTimeout)},
		Gates: Gates{
			Before: []Gate{{Type: GateChecks, Name: "checks", Timeout: durPtr(30 * time.Minute)}},
			After:  []Gate{{Type: GateHealth, Name: "health", Timeout: durPtr(10 * time.Minute)}},
		},
	}
}

func TestFileCatalogDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*App)
		want   func(*App)
	}{
		{name: "minimal app", mutate: func(*App) {}, want: func(*App) {}},
		{
			name:   "explicit branches kept",
			mutate: func(a *App) { a.Source.Branch = "trunk"; a.GitOps.Branch = "deploy" },
			want:   func(a *App) { a.Source.Branch = "trunk"; a.GitOps.Branch = "deploy" },
		},
		{
			name:   "gitops path cleaned",
			mutate: func(a *App) { a.GitOps.Path = "./apps//blog/" },
			want:   func(a *App) { a.GitOps.Path = "apps/blog" },
		},
		{
			name:   "dot gitops path is the root",
			mutate: func(a *App) { a.GitOps.Path = "." },
			want:   func(a *App) { a.GitOps.Path = "" },
		},
		{
			name:   "empty gitops path is the root",
			mutate: func(a *App) { a.GitOps.Path = "" },
			want:   func(a *App) { a.GitOps.Path = "" },
		},
		{
			name:   "empty kustomize image uses image repository",
			mutate: func(a *App) { a.Pin.Kustomize = &KustomizePin{} },
			want:   func(*App) {},
		},
		{
			name:   "explicit kustomize image kept",
			mutate: func(a *App) { a.Pin.Kustomize = &KustomizePin{Image: "blog"} },
			want:   func(a *App) { a.Pin.Kustomize = &KustomizePin{Image: "blog"} },
		},
		{
			name:   "yaml pin defaults format and cleans file",
			mutate: func(a *App) { a.Pin.YAML = &YAMLPin{File: "./values.yaml", Path: "image.digest"} },
			want: func(a *App) {
				a.Pin = Pin{YAML: &YAMLPin{File: "values.yaml", Path: "image.digest", Format: PinFormatDigest}}
			},
		},
		{
			name:   "yaml pin reference format kept",
			mutate: func(a *App) { a.Pin.YAML = &YAMLPin{File: "d.yaml", Path: "a", Format: PinFormatReference} },
			want: func(a *App) {
				a.Pin = Pin{YAML: &YAMLPin{File: "d.yaml", Path: "a", Format: PinFormatReference}}
			},
		},
		{
			name: "both pin strategies kept for validation",
			mutate: func(a *App) {
				a.Pin = Pin{Kustomize: &KustomizePin{}, YAML: &YAMLPin{File: "d.yaml", Path: "a"}}
			},
			want: func(a *App) {
				a.Pin = Pin{
					Kustomize: &KustomizePin{Image: "ghcr.io/example-user/blog"},
					YAML:      &YAMLPin{File: "d.yaml", Path: "a", Format: PinFormatDigest},
				}
			},
		},
		{
			name:   "pull request mode kept",
			mutate: func(a *App) { a.Deploy.Mode = DeployPullRequest },
			want:   func(a *App) { a.Deploy.Mode = DeployPullRequest },
		},
		{
			name:   "explicit max batch kept",
			mutate: func(a *App) { a.Deploy.MaxBatch = intPtr(1) },
			want:   func(a *App) { a.Deploy.MaxBatch = intPtr(1) },
		},
		{
			name:   "invalid max batch kept for validation",
			mutate: func(a *App) { a.Deploy.MaxBatch = intPtr(0) },
			want:   func(a *App) { a.Deploy.MaxBatch = intPtr(0) },
		},
		{
			name:   "migrations block enables the trailer by default",
			mutate: func(a *App) { a.Migrations = &Migrations{Labels: []string{"db-migration"}} },
			want:   func(a *App) { a.Migrations = &Migrations{Labels: []string{"db-migration"}, Trailer: new(true)} },
		},
		{
			name:   "migrations trailer can be disabled",
			mutate: func(a *App) { a.Migrations = &Migrations{Labels: []string{"db"}, Trailer: new(false)} },
			want:   func(a *App) { a.Migrations = &Migrations{Labels: []string{"db"}, Trailer: new(false)} },
		},
		{
			name: "backup gate defaults name, when and timeout",
			mutate: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateChecks}, {Type: GateBackup, Webhook: &GateWebhook{URLEnv: "BACKUP_URL"}}}
			},
			want: func(a *App) {
				a.Gates.Before = append(a.Gates.Before, Gate{
					Type: GateBackup, Name: "backup", When: GateWhenMigration, Timeout: durPtr(DefaultBackupTimeout),
					Webhook: &GateWebhook{URLEnv: "BACKUP_URL"},
				})
			},
		},
		{
			name: "backup gate keeps an explicit when",
			mutate: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateChecks}, {Type: GateBackup, Name: "snapshot", When: GateWhenAlways, Timeout: durPtr(time.Hour), ApplyToRollbacks: true}}
			},
			want: func(a *App) {
				a.Gates.Before = append(a.Gates.Before, Gate{Type: GateBackup, Name: "snapshot", When: GateWhenAlways, Timeout: durPtr(time.Hour), ApplyToRollbacks: true})
			},
		},
		{
			name: "argocd overrides kept",
			mutate: func(a *App) {
				a.ArgoCD = ArgoCD{Application: "web", Namespace: "gitops", SyncTimeout: durPtr(time.Minute)}
			},
			want: func(a *App) {
				a.ArgoCD = ArgoCD{Application: "web", Namespace: "gitops", SyncTimeout: durPtr(time.Minute)}
			},
		},
		{
			name:   "invalid sync timeout kept for validation",
			mutate: func(a *App) { a.ArgoCD.SyncTimeout = durPtr(-time.Minute) },
			want:   func(a *App) { a.ArgoCD.SyncTimeout = durPtr(-time.Minute) },
		},
		{
			name:   "only after gates given keeps default before gates",
			mutate: func(a *App) { a.Gates.After = []Gate{{Type: GateHealth}} },
			want: func(a *App) {
				a.Gates.After = []Gate{{Type: GateHealth, Name: "health", Timeout: durPtr(10 * time.Minute)}}
			},
		},
		{
			name:   "explicit empty phases disable gates",
			mutate: func(a *App) { a.Gates = Gates{Before: []Gate{}, After: []Gate{}} },
			want:   func(a *App) { a.Gates = Gates{Before: []Gate{}, After: []Gate{}} },
		},
		{
			name: "gate names and timeouts kept",
			mutate: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateChecks, Name: "ci", Timeout: durPtr(time.Hour)}}
			},
			want: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateChecks, Name: "ci", Timeout: durPtr(time.Hour)}}
			},
		},
		{
			name: "explicit zero timeout is kept for validation",
			mutate: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateChecks, Timeout: durPtr(0)}}
			},
			want: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateChecks, Name: "checks", Timeout: durPtr(0)}}
			},
		},
		{
			name:   "soak gate timeout defaults to duration plus allowance",
			mutate: func(a *App) { a.Gates.After = []Gate{{Type: GateSoak, Duration: Duration(time.Hour)}} },
			want: func(a *App) {
				a.Gates.After = []Gate{{Type: GateSoak, Name: "soak", Duration: Duration(time.Hour), Timeout: durPtr(70 * time.Minute)}}
			},
		},
		{
			name: "soak gate explicit timeout kept",
			mutate: func(a *App) {
				a.Gates.After = []Gate{{Type: GateSoak, Duration: Duration(time.Hour), Timeout: durPtr(3 * time.Hour)}}
			},
			want: func(a *App) {
				a.Gates.After = []Gate{{Type: GateSoak, Name: "soak", Duration: Duration(time.Hour), Timeout: durPtr(3 * time.Hour)}}
			},
		},
		{
			name: "job gate timeout defaults to ten minutes",
			mutate: func(a *App) {
				a.Gates.After = []Gate{{Type: GateJob, Job: &JobGate{Namespace: "blog", Name: "smoke-test"}}}
			},
			want: func(a *App) {
				a.Gates.After = []Gate{{Type: GateJob, Name: "job", Timeout: durPtr(10 * time.Minute), Job: &JobGate{Namespace: "blog", Name: "smoke-test"}}}
			},
		},
		{
			name:   "lock gate gets no timeout",
			mutate: func(a *App) { a.Gates.Before = []Gate{{Type: GateLock}} },
			want:   func(a *App) { a.Gates.Before = []Gate{{Type: GateLock, Name: "lock"}} },
		},
		{
			name: "schedule gate time zones default to UTC",
			mutate: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateSchedule, Windows: []Window{{Effect: WindowDeny, Cron: "0 17 * * 5", Duration: Duration(63 * time.Hour)}}}}
			},
			want: func(a *App) {
				a.Gates.Before = []Gate{{
					Type: GateSchedule, Name: "schedule", Timezone: "UTC",
					Windows: []Window{{Effect: WindowDeny, Cron: "0 17 * * 5", Duration: Duration(63 * time.Hour), Timezone: "UTC"}},
				}}
			},
		},
		{
			name: "schedule windows inherit the gate time zone",
			mutate: func(a *App) {
				a.Gates.Before = []Gate{{
					Type: GateSchedule, Timezone: "Europe/London", ApplyToRollbacks: true,
					Windows: []Window{
						{Effect: WindowAllow, Cron: "0 9 * * 1-5", Duration: Duration(8 * time.Hour)},
						{Effect: WindowDeny, Cron: "0 0 24 12 *", Duration: Duration(48 * time.Hour), Timezone: "America/New_York", Name: "holidays"},
					},
				}}
			},
			want: func(a *App) {
				a.Gates.Before = []Gate{{
					Type: GateSchedule, Name: "schedule", Timezone: "Europe/London", ApplyToRollbacks: true,
					Windows: []Window{
						{Effect: WindowAllow, Cron: "0 9 * * 1-5", Duration: Duration(8 * time.Hour), Timezone: "Europe/London"},
						{Effect: WindowDeny, Cron: "0 0 24 12 *", Duration: Duration(48 * time.Hour), Timezone: "America/New_York", Name: "holidays"},
					},
				}}
			},
		},
		{
			name:   "external gate timeout defaults to a day",
			mutate: func(a *App) { a.Gates.After = []Gate{{Type: GateExternal}} },
			want: func(a *App) {
				a.Gates.After = []Gate{{Type: GateExternal, Name: "external", Timeout: durPtr(DefaultExternalGateTimeout)}}
			},
		},
		{
			name: "external gate timeout kept",
			mutate: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateExternal, Name: "approval", Timeout: durPtr(48 * time.Hour), ApplyToRollbacks: true, Reporters: []string{"token:cab"}}}
			},
			want: func(a *App) {
				a.Gates.Before = []Gate{{Type: GateExternal, Name: "approval", Timeout: durPtr(48 * time.Hour), ApplyToRollbacks: true, Reporters: []string{"token:cab"}}}
			},
		},
		{
			name:   "unknown gate type gets name only",
			mutate: func(a *App) { a.Gates.Before = []Gate{{Type: "metric"}} },
			want:   func(a *App) { a.Gates.Before = []Gate{{Type: "metric", Name: "metric"}} },
		},
		{
			name:   "deployments switch kept",
			mutate: func(a *App) { a.Source.Deployments = new(false) },
			want:   func(a *App) { a.Source.Deployments = new(false) },
		},
		{
			name:   "statuses switch kept",
			mutate: func(a *App) { a.Source.Statuses = new(false) },
			want:   func(a *App) { a.Source.Statuses = new(false) },
		},
		{
			name:   "notify kept",
			mutate: func(a *App) { a.Notify = []string{"ops"} },
			want:   func(a *App) { a.Notify = []string{"ops"} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := minimalApp()
			tt.mutate(&app)
			want := defaultedApp()
			tt.want(&want)

			got := File{Apps: []App{app}}.Catalog()
			if len(got.Apps) != 1 {
				t.Fatalf("Catalog() has %d apps, want 1", len(got.Apps))
			}
			if !reflect.DeepEqual(got.Apps[0], want) {
				t.Errorf("Catalog() app =\n%+v\nwant\n%+v", got.Apps[0], want)
			}
		})
	}
}

func TestFileCatalogDoesNotAlias(t *testing.T) {
	t.Parallel()

	app := minimalApp()
	app.Source.RequiredChecks = []string{"test"}
	app.Pin.YAML = &YAMLPin{File: "values.yaml", Path: "image.digest"}
	app.Gates.Before = []Gate{{Type: GateChecks, Timeout: durPtr(time.Hour)}}
	app.Gates.Before = append(app.Gates.Before, Gate{Type: GateSchedule, Windows: []Window{{Effect: WindowDeny, Cron: "0 17 * * 5", Duration: Duration(time.Hour)}}})
	app.Gates.After = []Gate{{Type: GateHealth}, {Type: GateJob, Job: &JobGate{Namespace: "blog", Name: "smoke"}}}
	app.Notify = []string{"ops"}
	app.Source.Deployments = new(true)
	app.Source.Statuses = new(true)
	app.ArgoCD.SyncTimeout = durPtr(time.Minute)
	app.Migrations = &Migrations{Labels: []string{"db-migration"}}
	app.Gates.Before = append(app.Gates.Before, Gate{Type: GateBackup, Webhook: &GateWebhook{URLEnv: "BACKUP_URL"}})
	file := File{
		Apps:          []App{app},
		Notifications: []Notification{{Name: "ops", Type: NotifySlack, WebhookURLEnv: "X", Events: []string{"deploy.failed"}}},
	}

	catalog := file.Catalog()
	catalog.Apps[0].Source.RequiredChecks[0] = "changed"
	catalog.Apps[0].Pin.YAML.Path = "changed"
	catalog.Apps[0].Gates.After[0].Name = "changed"
	catalog.Apps[0].Gates.After[1].Job.Name = "changed"
	*catalog.Apps[0].Gates.Before[0].Timeout = 0
	catalog.Apps[0].Gates.Before[1].Windows[0].Cron = "changed"
	catalog.Apps[0].Notify[0] = "changed"
	catalog.Notifications[0].Name = "changed"
	catalog.Notifications[0].Events[0] = "changed"
	*catalog.Apps[0].Source.Deployments = false
	*catalog.Apps[0].Source.Statuses = false
	*catalog.Apps[0].ArgoCD.SyncTimeout = 0
	catalog.Apps[0].Migrations.Labels[0] = "changed"
	catalog.Apps[0].Gates.Before[2].Webhook.URLEnv = "CHANGED"

	original := file.Apps[0]
	if original.Migrations.Labels[0] != "db-migration" || original.Migrations.Trailer != nil || original.Gates.Before[2].Webhook.URLEnv != "BACKUP_URL" {
		t.Errorf("Catalog() shares migration or backup settings with the file: %+v", original)
	}
	if original.Source.RequiredChecks[0] != "test" || original.Pin.YAML.Path != "image.digest" ||
		original.Gates.After[0].Name != "" || original.Gates.After[1].Job.Name != "smoke" || *original.Gates.Before[0].Timeout != Duration(time.Hour) ||
		original.Gates.Before[1].Windows[0].Cron != "0 17 * * 5" || original.Gates.Before[1].Windows[0].Timezone != "" ||
		original.Notify[0] != "ops" || file.Notifications[0].Name != "ops" ||
		file.Notifications[0].Events[0] != "deploy.failed" || !*original.Source.Deployments || !*original.Source.Statuses || *original.ArgoCD.SyncTimeout != Duration(time.Minute) {
		t.Errorf("Catalog() shares memory with the file: %+v", file)
	}
}

func TestParseScheduleWindowsWithAnchors(t *testing.T) {
	t.Parallel()

	const yaml = `
apps:
  - name: blog
    source: {repo: example-user/blog}
    image: {repository: ghcr.io/example-user/blog}
    gitops: {repo: example-user/homelab, path: apps/blog}
    gates:
      before:
        - type: checks
        - type: schedule
          timezone: Europe/London
          windows: &business-hours
            - effect: allow
              cron: "0 9 * * 1-5"
              duration: 8h
              name: business hours
            - effect: deny
              cron: "0 0 24 12 *"
              duration: 48h
              timezone: America/New_York
  - name: api
    source: {repo: example-user/api}
    image: {repository: ghcr.io/example-user/api}
    gitops: {repo: example-user/homelab, path: apps/api}
    gates:
      before:
        - type: schedule
          apply_to_rollbacks: true
          windows: *business-hours
`
	f, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := f.Catalog()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	blog := c.Apps[0].Gates.Before[1]
	api := c.Apps[1].Gates.Before[0]
	wantBlog := []Window{
		{Effect: WindowAllow, Cron: "0 9 * * 1-5", Duration: Duration(8 * time.Hour), Timezone: "Europe/London", Name: "business hours"},
		{Effect: WindowDeny, Cron: "0 0 24 12 *", Duration: Duration(48 * time.Hour), Timezone: "America/New_York"},
	}
	wantAPI := []Window{
		{Effect: WindowAllow, Cron: "0 9 * * 1-5", Duration: Duration(8 * time.Hour), Timezone: "UTC", Name: "business hours"},
		{Effect: WindowDeny, Cron: "0 0 24 12 *", Duration: Duration(48 * time.Hour), Timezone: "America/New_York"},
	}
	if !reflect.DeepEqual(blog.Windows, wantBlog) {
		t.Errorf("blog windows = %+v, want %+v", blog.Windows, wantBlog)
	}
	if !reflect.DeepEqual(api.Windows, wantAPI) {
		t.Errorf("api windows = %+v, want %+v", api.Windows, wantAPI)
	}
	if blog.ApplyToRollbacks || !api.ApplyToRollbacks {
		t.Errorf("apply_to_rollbacks = %v, %v; want false, true", blog.ApplyToRollbacks, api.ApplyToRollbacks)
	}
}

func TestParseRejectsUnknownWindowField(t *testing.T) {
	t.Parallel()

	const yaml = "apps:\n  - name: blog\n    gates:\n      before:\n        - type: schedule\n          windows:\n            - effect: deny\n              schedule: \"0 17 * * 5\"\n"
	if _, err := Parse(strings.NewReader(yaml)); err == nil || !strings.Contains(err.Error(), "schedule") {
		t.Errorf("Parse() error = %v, want unknown field schedule", err)
	}
}

func TestFileCatalogEmpty(t *testing.T) {
	t.Parallel()

	got := File{}.Catalog()
	if len(got.Apps) != 0 || len(got.Notifications) != 0 {
		t.Errorf("Catalog() = %+v, want empty", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("empty catalog Validate() = %v", err)
	}
}

func TestParseEmptyEventsIsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{name: "omitted", yaml: "notifications:\n  - {name: ops, type: slack, webhook_url_env: X}\n"},
		{name: "empty list", yaml: "notifications:\n  - {name: ops, type: slack, webhook_url_env: X, events: []}\n", wantErr: true},
		{name: "null", yaml: "notifications:\n  - {name: ops, type: slack, webhook_url_env: X, events: null}\n"},
		{name: "listed", yaml: "notifications:\n  - {name: ops, type: slack, webhook_url_env: X, events: [deploy.failed]}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse(strings.NewReader(tt.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = f.Catalog().Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestArgoCDTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   ArgoCD
		want time.Duration
	}{
		{name: "default", in: ArgoCD{}, want: core.DefaultSyncTimeout},
		{name: "explicit", in: ArgoCD{SyncTimeout: durPtr(3 * time.Minute)}, want: 3 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.in.Timeout(); got != tt.want {
				t.Fatalf("Timeout() = %s, want %s", got, tt.want)
			}
		})
	}
}
