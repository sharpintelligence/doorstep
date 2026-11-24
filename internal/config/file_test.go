package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const fullFile = `
server:
  listen_addr: 127.0.0.1:9000
  metrics_listen_addr: 127.0.0.1:9001
  shutdown_delay: 0s
  shutdown_timeout: 1m
log:
  level: debug
  format: text
database:
  url_file: /etc/doorstep/secrets/database-url
github:
  app_id: "123"
  private_key_file: /etc/doorstep/secrets/github-app.pem
  api_url: https://ghe.example.com/api/v3
registry:
  credentials_file: /etc/doorstep/registry/.dockerconfigjson
notifications:
  - name: ops
    type: slack
    webhook_url_env: SLACK_WEBHOOK_URL
  - name: hook
    type: webhook
    url: https://hooks.example.com/doorstep
    secret_file: /etc/doorstep/secrets/hook-secret
apps:
  - name: blog
    source:
      repo: example-user/blog
      branch: trunk
      required_checks: [test, lint]
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
    gates:
      before:
        - type: checks
          timeout: 45m
      after:
        - type: health
          name: argo-health
        - type: soak
          duration: 15m
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
		},
		Log:      FileLog{Level: strPtr("debug"), Format: strPtr("text")},
		Database: FileDatabase{URLFile: strPtr("/etc/doorstep/secrets/database-url")},
		GitHub: FileGitHub{
			AppID:          strPtr("123"),
			PrivateKeyFile: strPtr("/etc/doorstep/secrets/github-app.pem"),
			APIURL:         strPtr("https://ghe.example.com/api/v3"),
		},
		Registry: FileRegistry{CredentialsFile: strPtr("/etc/doorstep/registry/.dockerconfigjson")},
		Notifications: []Notification{
			{Name: "ops", Type: NotifySlack, WebhookURLEnv: "SLACK_WEBHOOK_URL"},
			{Name: "hook", Type: NotifyWebhook, URL: "https://hooks.example.com/doorstep", SecretFile: "/etc/doorstep/secrets/hook-secret"},
		},
		Apps: []App{{
			Name:   "blog",
			Source: Source{Repo: "example-user/blog", Branch: "trunk", RequiredChecks: []string{"test", "lint"}},
			Image:  Image{Repository: "ghcr.io/example-user/blog"},
			GitOps: GitOps{Repo: "example-org/homelab", Branch: "deploy", Path: "apps/blog/"},
			Pin:    Pin{YAML: &YAMLPin{File: "values.yaml", Path: "image.digest"}},
			Deploy: Deploy{Mode: DeployPullRequest, MaxBatch: intPtr(5)},
			ArgoCD: ArgoCD{Application: "blog-prod", Namespace: "gitops"},
			Gates: Gates{
				Before: []Gate{{Type: GateChecks, Timeout: durPtr(45 * time.Minute)}},
				After: []Gate{
					{Type: GateHealth, Name: "argo-health"},
					{Type: GateSoak, Duration: Duration(15 * time.Minute)},
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
			name: "all settings",
			file: File{
				Server:   FileServer{ListenAddr: strPtr("127.0.0.1:1"), MetricsListenAddr: strPtr("127.0.0.1:2"), ShutdownDelay: durPtr(time.Second), ShutdownTimeout: durPtr(2 * time.Second)},
				Log:      FileLog{Level: strPtr("warn"), Format: strPtr("text")},
				Database: FileDatabase{URLFile: strPtr("/secrets/db")},
				GitHub:   FileGitHub{AppID: strPtr("42"), PrivateKeyFile: strPtr("/secrets/key.pem"), APIURL: strPtr("https://ghe.example.com/api/v3")},
				Registry: FileRegistry{CredentialsFile: strPtr("/secrets/config.json")},
			},
			want: func(c *Config) {
				c.Server = Server{ListenAddr: "127.0.0.1:1", MetricsListenAddr: "127.0.0.1:2", ShutdownDelay: time.Second, ShutdownTimeout: 2 * time.Second}
				c.Log = Log{Level: "warn", Format: "text"}
				c.Database = Database{URLFile: "/secrets/db"}
				c.GitHub = GitHub{AppID: "42", PrivateKeyFile: "/secrets/key.pem", APIURL: "https://ghe.example.com/api/v3"}
				c.Registry = Registry{CredentialsFile: "/secrets/config.json"}
			},
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
		ArgoCD: ArgoCD{Application: "blog", Namespace: "argocd"},
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
			name:   "argocd overrides kept",
			mutate: func(a *App) { a.ArgoCD = ArgoCD{Application: "web", Namespace: "gitops"} },
			want:   func(a *App) { a.ArgoCD = ArgoCD{Application: "web", Namespace: "gitops"} },
		},
		{
			name:   "only after gates given keeps default before gates",
			mutate: func(a *App) { a.Gates.After = []Gate{{Type: GateSoak, Duration: Duration(time.Minute)}} },
			want: func(a *App) {
				a.Gates.After = []Gate{{Type: GateSoak, Name: "soak", Duration: Duration(time.Minute)}}
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
			name:   "soak gate gets no default timeout",
			mutate: func(a *App) { a.Gates.After = []Gate{{Type: GateSoak, Duration: Duration(time.Minute)}} },
			want: func(a *App) {
				a.Gates.After = []Gate{{Type: GateSoak, Name: "soak", Duration: Duration(time.Minute)}}
			},
		},
		{
			name:   "unknown gate type gets name only",
			mutate: func(a *App) { a.Gates.Before = []Gate{{Type: "metric"}} },
			want:   func(a *App) { a.Gates.Before = []Gate{{Type: "metric", Name: "metric"}} },
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
	app.Gates.After = []Gate{{Type: GateHealth}}
	app.Notify = []string{"ops"}
	file := File{
		Apps:          []App{app},
		Notifications: []Notification{{Name: "ops", Type: NotifySlack, WebhookURLEnv: "X"}},
	}

	catalog := file.Catalog()
	catalog.Apps[0].Source.RequiredChecks[0] = "changed"
	catalog.Apps[0].Pin.YAML.Path = "changed"
	catalog.Apps[0].Gates.After[0].Name = "changed"
	*catalog.Apps[0].Gates.Before[0].Timeout = 0
	catalog.Apps[0].Notify[0] = "changed"
	catalog.Notifications[0].Name = "changed"

	original := file.Apps[0]
	if original.Source.RequiredChecks[0] != "test" || original.Pin.YAML.Path != "image.digest" ||
		original.Gates.After[0].Name != "" || *original.Gates.Before[0].Timeout != Duration(time.Hour) || original.Notify[0] != "ops" || file.Notifications[0].Name != "ops" {
		t.Errorf("Catalog() shares memory with the file: %+v", file)
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
