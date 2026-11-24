package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/github"
	"github.com/sharpintelligence/doorstep/internal/github/githubtest"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/notify"
)

const (
	notifyTestSHA = "0123456789abcdef0123456789abcdef01234567"
	slackPath     = "/services/T000/B000/s3cr3t-t0k3n"
)

func TestNotificationEventsMatchCore(t *testing.T) {
	t.Parallel()

	toStrings := func(kinds []core.NotificationKind) []string {
		out := make([]string, len(kinds))
		for i, k := range kinds {
			out[i] = k.String()
		}
		return out
	}
	if got, want := config.NotificationEvents(), toStrings(core.NotificationKinds()); !slices.Equal(got, want) {
		t.Errorf("config.NotificationEvents() = %v, core.NotificationKinds() = %v", got, want)
	}
	if got, want := config.DefaultNotificationEvents(), toStrings(core.DefaultNotificationKinds()); !slices.Equal(got, want) {
		t.Errorf("config.DefaultNotificationEvents() = %v, core.DefaultNotificationKinds() = %v", got, want)
	}
}

type request struct {
	path   string
	header http.Header
	body   []byte
}

type endpoint struct {
	*httptest.Server
	mu       sync.Mutex
	requests []request
	status   atomic.Int32
}

func newEndpoint(t *testing.T) *endpoint {
	t.Helper()
	e := &endpoint{}
	e.status.Store(http.StatusOK)
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.requests = append(e.requests, request{path: r.URL.Path, header: r.Header.Clone(), body: body})
		e.mu.Unlock()
		w.WriteHeader(int(e.status.Load()))
	}))
	t.Cleanup(e.Close)
	return e
}

func (e *endpoint) got() []request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.requests)
}

type mutableEnv struct {
	mu    sync.Mutex
	vars  map[string]string
	reads atomic.Int32
}

func (m *mutableEnv) lookup(key string) (string, bool) {
	m.reads.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vars[key]
	return v, ok
}

func (m *mutableEnv) set(key, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vars[key] = value
}

func deployNotification(app string, kind core.NotificationKind, id core.DeploymentID) core.Notification {
	target := core.Target{App: app, Environment: app}
	return core.Notification{
		Kind:   kind,
		Target: target,
		Deployment: &core.Deployment{
			ID: id, Target: target, Kind: core.KindDeploy, State: core.StateSucceeded, Source: core.SourceWebhook,
			Commit: core.Commit{Repository: "example-user/" + app, SHA: notifyTestSHA},
		},
		OccurredAt: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC),
	}
}

func closeNotifier(t *testing.T, d *notify.Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBuildNotifierRoutesAndFilters(t *testing.T) {
	t.Parallel()

	slack := newEndpoint(t)
	hooks := newEndpoint(t)
	env := &mutableEnv{vars: map[string]string{"SLACK_URL": slack.URL + slackPath, "HOOK_SECRET": "first"}}
	var fileReads atomic.Int32
	readFile := func(name string) ([]byte, error) {
		fileReads.Add(1)
		if name == "/etc/doorstep/hook-url" {
			return []byte(hooks.URL + "/doorstep\n"), nil
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	catalog := config.File{
		Server: config.FileServer{},
		Notifications: []config.Notification{
			{Name: "ops", Type: config.NotifySlack, WebhookURLEnv: "SLACK_URL"},
			{Name: "hooks", Type: config.NotifyWebhook, URLFile: "/etc/doorstep/hook-url", SecretEnv: "HOOK_SECRET", Events: []string{"deploy.failed", "deploy.queued"}},
		},
		Apps: []config.App{
			{Name: "blog", Notify: []string{"ops", "hooks"}},
			{Name: "api", Notify: []string{"hooks"}},
			{Name: "quiet"},
		},
	}.Catalog()
	cfg := config.Default()
	cfg.Server.PublicURL = "https://doorstep.example.com/"

	d, err := buildNotifier(notifierInput{
		catalog:    catalog,
		config:     cfg,
		secrets:    secretSource{lookupEnv: env.lookup, readFile: readFile},
		httpClient: notify.NewHTTPClient(),
		userAgent:  "doorstep/test",
	})
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	d.Notify(t.Context(), deployNotification("blog", core.NotificationDeploySucceeded, 1))
	d.Notify(t.Context(), deployNotification("blog", core.NotificationDeployFailed, 2))
	d.Notify(t.Context(), deployNotification("api", core.NotificationDeployQueued, 3))
	d.Notify(t.Context(), deployNotification("api", core.NotificationDeploySucceeded, 4))
	d.Notify(t.Context(), deployNotification("quiet", core.NotificationDeployFailed, 5))
	closeNotifier(t, d)

	slackReqs := slack.got()
	if len(slackReqs) != 2 {
		t.Fatalf("slack received %d requests, want 2 (blog succeeded and failed)", len(slackReqs))
	}
	for _, r := range slackReqs {
		if r.path != slackPath || r.header.Get("User-Agent") != "doorstep/test" {
			t.Errorf("slack request = %s %v", r.path, r.header)
		}
	}
	if !strings.Contains(string(slackReqs[0].body), "https://doorstep.example.com/deployments/1|View deployment") {
		t.Errorf("slack message has no deployment link: %s", slackReqs[0].body)
	}
	if !strings.Contains(string(slackReqs[0].body), "https://github.com/example-user/blog/commit/"+notifyTestSHA) {
		t.Errorf("slack message has no commit link: %s", slackReqs[0].body)
	}

	hookReqs := hooks.got()
	var events []string
	for _, r := range hookReqs {
		events = append(events, r.header.Get(notify.HeaderEvent))
		if r.header.Get(notify.HeaderSignature) != notify.Sign([]byte("first"), r.body) {
			t.Errorf("webhook signature = %q", r.header.Get(notify.HeaderSignature))
		}
	}
	slices.Sort(events)
	if want := []string{"deploy.failed", "deploy.queued"}; !slices.Equal(events, want) {
		t.Errorf("webhook events = %v, want %v", events, want)
	}
	if fileReads.Load() != 2 {
		t.Errorf("webhook URL file read %d times, want once per delivery", fileReads.Load())
	}
}

func TestBuildNotifierRereadsSecrets(t *testing.T) {
	t.Parallel()

	first, second := newEndpoint(t), newEndpoint(t)
	env := &mutableEnv{vars: map[string]string{"HOOK_URL": first.URL, "HOOK_SECRET": "one"}}
	catalog := config.File{
		Notifications: []config.Notification{{Name: "hooks", Type: config.NotifyWebhook, URLEnv: "HOOK_URL", SecretEnv: "HOOK_SECRET"}},
		Apps:          []config.App{{Name: "blog", Notify: []string{"hooks"}}},
	}.Catalog()
	build := func() *notify.Dispatcher {
		d, err := buildNotifier(notifierInput{catalog: catalog, config: config.Default(), secrets: secretSource{lookupEnv: env.lookup, readFile: config.ReadSecretFile}})
		if err != nil {
			t.Fatalf("buildNotifier: %v", err)
		}
		return d
	}
	d := build()
	d.Notify(t.Context(), deployNotification("blog", core.NotificationDeployFailed, 1))
	closeNotifier(t, d)
	env.set("HOOK_URL", second.URL)
	env.set("HOOK_SECRET", "two")
	d = build()
	d.Notify(t.Context(), deployNotification("blog", core.NotificationDeployFailed, 2))
	closeNotifier(t, d)

	a, b := first.got(), second.got()
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("requests = %d, %d; want one each", len(a), len(b))
	}
	if b[0].header.Get(notify.HeaderSignature) != notify.Sign([]byte("two"), b[0].body) {
		t.Error("rotated secret not used")
	}
}

func TestBuildNotifierGitHubDeployments(t *testing.T) {
	t.Parallel()

	srv := githubtest.NewServer(t)
	srv.Install("example-user", "blog", 1)
	srv.Install("example-user", "api", 1)
	var mu sync.Mutex
	created := map[string]int{}
	srv.Handle("GET /repos/{owner}/{repo}/deployments", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		githubtest.WriteJSON(w, http.StatusOK, []any{})
	}))
	srv.Handle("POST /repos/{owner}/{repo}/deployments", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		created[r.PathValue("repo")]++
		mu.Unlock()
		githubtest.WriteJSON(w, http.StatusCreated, map[string]any{"id": 7})
	}))
	srv.Handle("POST /repos/{owner}/{repo}/deployments/{id}/statuses", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		githubtest.WriteJSON(w, http.StatusCreated, map[string]any{"id": 1})
	}))
	app, err := github.New(github.Options{AppID: srv.AppID, PrivateKey: srv.PrivateKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	catalog := config.File{Apps: []config.App{
		{Name: "blog"},
		{Name: "api", Source: config.Source{Deployments: new(false)}},
	}}.Catalog()
	cfg := config.Default()
	cfg.GitHub.APIURL = srv.URL
	m := metrics.New(metrics.Options{})

	d, err := newNotifier(catalog, cfg, envFrom(nil), app, nil, m.Recorder())
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}
	d.Notify(t.Context(), deployNotification("blog", core.NotificationDeployStarted, 1))
	d.Notify(t.Context(), deployNotification("api", core.NotificationDeployStarted, 2))
	closeNotifier(t, d)

	mu.Lock()
	defer mu.Unlock()
	if created["blog"] != 1 || created["api"] != 0 {
		t.Errorf("deployments created = %v, want only blog", created)
	}
	want := `
# HELP doorstep_notifications_total Notification deliveries, by target type and result.
# TYPE doorstep_notifications_total counter
doorstep_notifications_total{result="dropped",type="github"} 0
doorstep_notifications_total{result="dropped",type="slack"} 0
doorstep_notifications_total{result="dropped",type="webhook"} 0
doorstep_notifications_total{result="failed",type="github"} 0
doorstep_notifications_total{result="failed",type="slack"} 0
doorstep_notifications_total{result="failed",type="webhook"} 0
doorstep_notifications_total{result="sent",type="github"} 1
doorstep_notifications_total{result="sent",type="slack"} 0
doorstep_notifications_total{result="sent",type="webhook"} 0
`
	if err := testutil.GatherAndCompare(m.Gatherer(), strings.NewReader(want), "doorstep_notifications_total"); err != nil {
		t.Error(err)
	}
}

func TestNewNotifierWithoutGitHubOrMetrics(t *testing.T) {
	t.Parallel()

	d, err := newNotifier(config.Catalog{}, config.Default(), envFrom(nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("newNotifier: %v", err)
	}
	d.Notify(t.Context(), deployNotification("blog", core.NotificationDeployStarted, 1))
	closeNotifier(t, d)
}

func TestBuildNotifierErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		catalog config.Catalog
		cfg     func(*config.Config)
		wantErr string
	}{
		{
			name:    "unknown event",
			catalog: config.Catalog{Notifications: []config.Notification{{Name: "ops", Type: config.NotifySlack, WebhookURLEnv: "X", Events: []string{"deploy.exploded"}}}},
			wantErr: `notification ops: unknown notification kind: "deploy.exploded"`,
		},
		{
			name:    "unknown type",
			catalog: config.Catalog{Notifications: []config.Notification{{Name: "ops", Type: "email"}}},
			wantErr: `notification ops: unknown type "email"`,
		},
		{
			name:    "route to unknown target",
			catalog: config.Catalog{Apps: []config.App{{Name: "blog", Notify: []string{"pager"}}}},
			wantErr: `unknown sink "pager"`,
		},
		{
			name:    "invalid api url",
			cfg:     func(c *config.Config) { c.GitHub.APIURL = "ftp://x" },
			wantErr: "invalid github api url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			_, err := buildNotifier(notifierInput{catalog: tt.catalog, config: cfg})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("buildNotifier() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNotificationLinks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		publicURL      string
		apiURL         string
		wantDeployment string
		wantCommit     string
	}{
		{
			name:           "github.com with public url",
			publicURL:      "https://doorstep.example.com/",
			apiURL:         github.DefaultBaseURL,
			wantDeployment: "https://doorstep.example.com/deployments/42",
			wantCommit:     "https://github.com/acme/app/commit/" + notifyTestSHA,
		},
		{
			name:           "enterprise server under a path",
			publicURL:      "http://homelab.lan/doorstep",
			apiURL:         "https://ghe.example.com/api/v3",
			wantDeployment: "http://homelab.lan/doorstep/deployments/42",
			wantCommit:     "https://ghe.example.com/acme/app/commit/" + notifyTestSHA,
		},
		{
			name:       "no public url",
			apiURL:     github.DefaultBaseURL,
			wantCommit: "https://github.com/acme/app/commit/" + notifyTestSHA,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Server.PublicURL = tt.publicURL
			cfg.GitHub.APIURL = tt.apiURL
			links, err := notificationLinks(cfg)
			if err != nil {
				t.Fatalf("notificationLinks: %v", err)
			}
			if tt.wantDeployment == "" {
				if links.DeploymentURL != nil {
					t.Error("deployment links without a public URL")
				}
			} else if got := links.DeploymentURL(42); got != tt.wantDeployment {
				t.Errorf("DeploymentURL(42) = %q, want %q", got, tt.wantDeployment)
			}
			if got := links.CommitURL("acme/app", notifyTestSHA); got != tt.wantCommit {
				t.Errorf("CommitURL() = %q, want %q", got, tt.wantCommit)
			}
		})
	}
}

func TestTestNotification(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	catalog := config.File{Apps: []config.App{
		{Name: "api", Source: config.Source{Repo: "example-org/api"}, Image: config.Image{Repository: "ghcr.io/example-org/api"}, Notify: []string{"other"}},
		{Name: "blog", Source: config.Source{Repo: "example-user/blog"}, Image: config.Image{Repository: "ghcr.io/example-user/blog"}, Notify: []string{"ops"}},
	}}.Catalog()

	tests := []struct {
		target, wantApp, wantRepo string
	}{
		{target: "ops", wantApp: "blog", wantRepo: "example-user/blog"},
		{target: "unused", wantApp: "doorstep-test", wantRepo: "example/doorstep-test"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			t.Parallel()
			n := testNotification(catalog, tt.target, now)
			if err := n.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if n.Kind != core.NotificationDeploySucceeded || n.Target.App != tt.wantApp || n.Deployment.Commit.Repository != tt.wantRepo {
				t.Errorf("testNotification() = %+v", n)
			}
			if n.Deployment.ID != 0 {
				t.Errorf("synthetic deployment has id %d, which could link to a real deployment", n.Deployment.ID)
			}
		})
	}
}

func notifyTestConfig(t *testing.T, hookURL string) string {
	t.Helper()
	return writeConfig(t, `
notifications:
  - name: ops
    type: slack
    webhook_url_env: TEST_SLACK_URL
  - name: hooks
    type: webhook
    url: `+hookURL+`
    secret_env: TEST_HOOK_SECRET
    events: [deploy.failed]
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    notify: [ops, hooks]
`)
}

func TestRunNotifyTest(t *testing.T) {
	t.Parallel()

	slack := newEndpoint(t)
	hooks := newEndpoint(t)
	broken := newEndpoint(t)
	broken.status.Store(http.StatusInternalServerError)
	path := notifyTestConfig(t, hooks.URL+"/doorstep")
	env := map[string]string{"TEST_SLACK_URL": slack.URL + slackPath, "TEST_HOOK_SECRET": "s"}

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string
		wantStderr string
		check      func(t *testing.T)
	}{
		{
			name:       "slack",
			args:       []string{"notify", "test", "ops", "--config", path},
			env:        env,
			wantStdout: "ok: sent deploy.succeeded to ops (slack)\n",
			check: func(t *testing.T) {
				t.Helper()
				reqs := slack.got()
				if len(reqs) != 1 || !strings.Contains(string(reqs[0].body), "blog: deploy succeeded") {
					t.Errorf("slack requests = %+v", reqs)
				}
			},
		},
		{
			name:       "webhook ignores its event filter and signs the payload",
			args:       []string{"notify", "test", "hooks"},
			env:        map[string]string{config.EnvConfig: path, "TEST_HOOK_SECRET": "s"},
			wantStdout: "ok: sent deploy.succeeded to hooks (webhook)\n",
			check: func(t *testing.T) {
				t.Helper()
				reqs := hooks.got()
				if len(reqs) != 1 || reqs[0].header.Get(notify.HeaderSignature) != notify.Sign([]byte("s"), reqs[0].body) {
					t.Fatalf("webhook requests = %+v", reqs)
				}
				var payload map[string]any
				if err := json.Unmarshal(reqs[0].body, &payload); err != nil || payload["type"] != "deploy.succeeded" || payload["app"] != "blog" {
					t.Errorf("payload = %s (%v)", reqs[0].body, err)
				}
			},
		},
		{
			name:       "target failure exits 1 without leaking the URL",
			args:       []string{"notify", "test", "ops", "--config", path},
			env:        map[string]string{"TEST_SLACK_URL": broken.URL + slackPath},
			wantCode:   1,
			wantStderr: "doorstep: notification ops (slack): unexpected response 500 Internal Server Error",
			check: func(t *testing.T) {
				t.Helper()
				if n := len(broken.got()); n != 1 {
					t.Errorf("attempts = %d, want a single attempt", n)
				}
			},
		},
		{
			name:       "unreadable secret",
			args:       []string{"notify", "test", "ops", "--config", path},
			wantCode:   1,
			wantStderr: "read slack webhook URL: notification ops webhook_url: environment variable TEST_SLACK_URL is not set",
		},
		{
			name:       "unknown target",
			args:       []string{"notify", "test", "pager", "--config", path},
			wantCode:   1,
			wantStderr: `unknown notification "pager" (configured: ops, hooks)`,
		},
		{
			name:       "no configuration",
			args:       []string{"notify", "test", "ops"},
			wantCode:   1,
			wantStderr: "no configuration file",
		},
		{
			name:       "invalid public url",
			args:       []string{"notify", "test", "ops", "--config", path},
			env:        map[string]string{config.EnvPublicURL: "nope"},
			wantCode:   1,
			wantStderr: "invalid configuration: public url",
		},
		{
			name:       "missing name",
			args:       []string{"notify", "test", "--config", path},
			wantCode:   1,
			wantStderr: "accepts 1 arg(s), received 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := Run(t.Context(), tt.args, &stdout, &stderr, envFrom(tt.env))
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stdout %q, stderr %q)", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
			if strings.Contains(stderr.String(), "s3cr3t") {
				t.Errorf("stderr leaks the webhook URL: %q", stderr.String())
			}
			if tt.check != nil {
				tt.check(t)
			}
		})
	}
}

func TestRunNotifyHelp(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"notify", "test", "--help"}, {"notify"}, {"serve", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Run(t.Context(), args, &stdout, &stderr, envFrom(nil)); code != 0 {
			t.Fatalf("%v: exit code = %d, stderr %q", args, code, stderr.String())
		}
		want := map[string][]string{
			"notify test": {"--config", config.EnvConfig, "deploy.succeeded", "single attempt"},
			"notify":      {"test"},
			"serve":       {"--public-url", config.EnvPublicURL},
		}[strings.Join(slices.DeleteFunc(slices.Clone(args), func(s string) bool { return s == "--help" }), " ")]
		for _, w := range want {
			if !strings.Contains(stdout.String(), w) {
				t.Errorf("%v help does not mention %q: %s", args, w, stdout.String())
			}
		}
	}
}
