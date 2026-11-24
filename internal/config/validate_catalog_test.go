package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func validCatalog() Catalog {
	return File{
		Notifications: []Notification{
			{Name: "ops", Type: NotifySlack, WebhookURLEnv: "SLACK_WEBHOOK_URL"},
			{Name: "hook", Type: NotifyWebhook, URL: "https://hooks.example.com/doorstep", SecretFile: "/etc/doorstep/hook-secret"},
		},
		Apps: []App{
			{
				Name:   "blog",
				Source: Source{Repo: "example-user/blog", RequiredChecks: []string{"test"}},
				Image:  Image{Repository: "ghcr.io/example-user/blog"},
				GitOps: GitOps{Repo: "example-user/homelab", Path: "apps/blog"},
				Notify: []string{"ops"},
			},
			{
				Name:   "api",
				Source: Source{Repo: "example-org/api"},
				Image:  Image{Repository: "registry.example.com:5000/team/api"},
				GitOps: GitOps{Repo: "example-org/platform", Branch: "deploy", Path: "charts/api"},
				Pin:    Pin{YAML: &YAMLPin{File: "values.yaml", Path: "image.digest"}},
				Deploy: Deploy{Mode: DeployPullRequest},
				Gates: Gates{After: []Gate{
					{Type: GateHealth},
					{Type: GateSoak, Duration: Duration(10 * time.Minute)},
				}},
				Notify: []string{"ops", "hook"},
			},
		},
	}.Catalog()
}

func scheduleGate() Gate {
	return Gate{
		Type:     GateSchedule,
		Name:     "windows",
		Timezone: "Europe/London",
		Windows: []Window{
			{Effect: WindowAllow, Cron: "0 9 * * 1-5", Duration: Duration(8 * time.Hour), Name: "business hours", Timezone: "Europe/London"},
			{Effect: WindowDeny, Cron: "0 0 24 12 *", Duration: Duration(48 * time.Hour), Timezone: "America/New_York"},
		},
	}
}

func TestCatalogValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Catalog)
		wantErr []string
	}{
		{name: "valid", mutate: func(*Catalog) {}},
		{name: "empty", mutate: func(c *Catalog) { *c = Catalog{} }},

		{
			name:    "notification name required",
			mutate:  func(c *Catalog) { c.Notifications[0].Name = "" },
			wantErr: []string{"notifications[0]: name: is required"},
		},
		{
			name:    "notification name not a DNS label",
			mutate:  func(c *Catalog) { c.Notifications[0].Name = "Ops_Team" },
			wantErr: []string{"notifications[0] (Ops_Team): name:", "DNS label"},
		},
		{
			name:    "notification name duplicate",
			mutate:  func(c *Catalog) { c.Notifications[1].Name = "ops" },
			wantErr: []string{"notifications[1] (ops): name: duplicate of notifications[0]"},
		},
		{
			name:    "notification type required",
			mutate:  func(c *Catalog) { c.Notifications[0].Type = "" },
			wantErr: []string{"notifications[0] (ops): type: is required"},
		},
		{
			name:    "notification type unknown",
			mutate:  func(c *Catalog) { c.Notifications[0].Type = "email" },
			wantErr: []string{`type: unknown type "email"`},
		},
		{
			name:    "slack without webhook url",
			mutate:  func(c *Catalog) { c.Notifications[0].WebhookURLEnv = "" },
			wantErr: []string{"notifications[0] (ops): webhook_url_env, webhook_url_file: set exactly one"},
		},
		{
			name:    "slack with both webhook sources",
			mutate:  func(c *Catalog) { c.Notifications[0].WebhookURLFile = "/etc/slack" },
			wantErr: []string{"webhook_url_env, webhook_url_file: set exactly one"},
		},
		{
			name: "slack webhook from file",
			mutate: func(c *Catalog) {
				c.Notifications[0].WebhookURLEnv = ""
				c.Notifications[0].WebhookURLFile = "/etc/slack"
			},
			wantErr: nil,
		},
		{
			name: "slack rejects webhook fields",
			mutate: func(c *Catalog) {
				n := &c.Notifications[0]
				n.URL, n.URLEnv, n.URLFile, n.SecretEnv, n.SecretFile = "https://x.example", "A", "/a", "B", "/b"
			},
			wantErr: []string{
				"url: not allowed for type slack",
				"url_env: not allowed for type slack",
				"url_file: not allowed for type slack",
				"secret_env: not allowed for type slack",
				"secret_file: not allowed for type slack",
			},
		},
		{
			name:    "slack webhook env name invalid",
			mutate:  func(c *Catalog) { c.Notifications[0].WebhookURLEnv = "1SLACK-URL" },
			wantErr: []string{`webhook_url_env: "1SLACK-URL" is not a valid environment variable name`},
		},
		{
			name: "slack webhook file relative",
			mutate: func(c *Catalog) {
				c.Notifications[0].WebhookURLEnv = ""
				c.Notifications[0].WebhookURLFile = "secrets/slack"
			},
			wantErr: []string{`webhook_url_file: "secrets/slack" must be an absolute path`},
		},
		{
			name:    "webhook without url",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "" },
			wantErr: []string{"notifications[1] (hook): url, url_env, url_file: set exactly one"},
		},
		{
			name:    "webhook with url and url_env",
			mutate:  func(c *Catalog) { c.Notifications[1].URLEnv = "HOOK_URL" },
			wantErr: []string{"url, url_env, url_file: set exactly one"},
		},
		{
			name:    "webhook url from env",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = ""; c.Notifications[1].URLEnv = "HOOK_URL" },
			wantErr: nil,
		},
		{
			name:    "webhook url file relative",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = ""; c.Notifications[1].URLFile = "hook" },
			wantErr: []string{`url_file: "hook" must be an absolute path`},
		},
		{
			name:    "webhook both secret sources",
			mutate:  func(c *Catalog) { c.Notifications[1].SecretEnv = "HOOK_SECRET" },
			wantErr: []string{"secret_env, secret_file: set at most one"},
		},
		{
			name:    "webhook secret env invalid",
			mutate:  func(c *Catalog) { c.Notifications[1].SecretFile = ""; c.Notifications[1].SecretEnv = "HOOK SECRET" },
			wantErr: []string{`secret_env: "HOOK SECRET" is not a valid environment variable name`},
		},
		{
			name:    "webhook without signing secret",
			mutate:  func(c *Catalog) { c.Notifications[1].SecretFile = "" },
			wantErr: nil,
		},
		{
			name: "webhook rejects slack fields",
			mutate: func(c *Catalog) {
				c.Notifications[1].WebhookURLEnv = "A"
				c.Notifications[1].WebhookURLFile = "/a"
			},
			wantErr: []string{"webhook_url_env: not allowed for type webhook", "webhook_url_file: not allowed for type webhook"},
		},
		{
			name:    "webhook url scheme",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "ftp://hooks.example.com" },
			wantErr: []string{"url: must use http or https"},
		},
		{
			name:    "webhook url without host",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "https:///path" },
			wantErr: []string{"url: must include a host"},
		},
		{
			name:    "webhook url not a URL",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "http://[::1" },
			wantErr: []string{"url: is not a valid URL"},
		},
		{
			name:    "webhook url with credentials",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "https://user:hunter2@hooks.example.com" },
			wantErr: []string{"url: must not include credentials"},
		},
		{
			name:    "webhook url with query token",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "https://hooks.example.com/x?token=abc" },
			wantErr: []string{"url: must not include a query string or fragment, which often carry tokens; use url_env or url_file"},
		},
		{
			name:    "webhook url with empty query",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "https://hooks.example.com/x?" },
			wantErr: []string{"url: must not include a query string or fragment"},
		},
		{
			name:    "webhook url with fragment",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "https://hooks.example.com/x#token=abc" },
			wantErr: []string{"url: must not include a query string or fragment"},
		},
		{
			name:    "plain http webhook url",
			mutate:  func(c *Catalog) { c.Notifications[1].URL = "http://ntfy.local:8080/doorstep" },
			wantErr: nil,
		},

		{
			name:    "app name required",
			mutate:  func(c *Catalog) { c.Apps[0].Name = "" },
			wantErr: []string{"apps[0]: name: is required"},
		},
		{
			name:    "app name uppercase",
			mutate:  func(c *Catalog) { c.Apps[0].Name = "Blog" },
			wantErr: []string{"apps[0] (Blog): name:", "DNS label"},
		},
		{
			name:    "app name too long",
			mutate:  func(c *Catalog) { c.Apps[0].Name = strings.Repeat("a", 64) },
			wantErr: []string{"at most 63 characters"},
		},
		{
			name:    "app name at length limit",
			mutate:  func(c *Catalog) { c.Apps[0].Name = strings.Repeat("a", 63) },
			wantErr: nil,
		},
		{
			name:    "app name leading hyphen",
			mutate:  func(c *Catalog) { c.Apps[0].Name = "-blog" },
			wantErr: []string{"DNS label"},
		},
		{
			name:    "app name duplicate",
			mutate:  func(c *Catalog) { c.Apps[1].Name = "blog" },
			wantErr: []string{"apps[1] (blog): name: duplicate of apps[0]"},
		},
		{
			name:    "source repo required",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "" },
			wantErr: []string{"apps[0] (blog): source.repo: is required"},
		},
		{
			name:    "source repo without owner",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "blog" },
			wantErr: []string{`source.repo: "blog" must have the form owner/name`},
		},
		{
			name:    "source repo with extra segment",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "a/b/c" },
			wantErr: []string{"must have the form owner/name"},
		},
		{
			name:    "source repo owner with double hyphen",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "example--user/blog" },
			wantErr: []string{`source.repo: owner "example--user"`},
		},
		{
			name:    "source repo owner too long",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = RepoName(strings.Repeat("a", 40) + "/blog") },
			wantErr: []string{"source.repo: owner"},
		},
		{
			name:    "source repo owner with underscore",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "example_user/blog" },
			wantErr: []string{"source.repo: owner"},
		},
		{
			name:    "source repo name with space",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "example-user/my blog" },
			wantErr: []string{`source.repo: name "my blog"`},
		},
		{
			name:    "source repo name dot dot",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "example-user/.." },
			wantErr: []string{`source.repo: name ".."`},
		},
		{
			name:    "source repo name too long",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = RepoName("example-user/" + strings.Repeat("b", 101)) },
			wantErr: []string{"source.repo: name"},
		},
		{
			name:    "source repo mixed case with dots",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Repo = "Example-User/My.Blog_v2" },
			wantErr: nil,
		},
		{
			name:    "source branch empty",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "" },
			wantErr: []string{"source.branch: is required"},
		},
		{
			name:    "source branch with space",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "my branch" },
			wantErr: []string{`source.branch: "my branch" is not a valid branch name`},
		},
		{
			name:    "source branch with dot dot",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "release..1" },
			wantErr: []string{"source.branch:"},
		},
		{
			name:    "source branch leading hyphen",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "-main" },
			wantErr: []string{"source.branch:"},
		},
		{
			name:    "source branch leading slash",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "/main" },
			wantErr: []string{"source.branch:"},
		},
		{
			name:    "source branch control character",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "ma\x7fin" },
			wantErr: []string{"source.branch:"},
		},
		{
			name:    "source branch with colon",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "a:b" },
			wantErr: []string{"source.branch:"},
		},
		{
			name:    "source branch with slashes",
			mutate:  func(c *Catalog) { c.Apps[0].Source.Branch = "release/v1.2" },
			wantErr: nil,
		},
		{
			name:    "required check empty",
			mutate:  func(c *Catalog) { c.Apps[0].Source.RequiredChecks = []string{"test", " "} },
			wantErr: []string{"source.required_checks[1]: must not be empty"},
		},
		{
			name:    "required check duplicate",
			mutate:  func(c *Catalog) { c.Apps[0].Source.RequiredChecks = []string{"test", "test"} },
			wantErr: []string{`source.required_checks[1]: duplicate check "test"`},
		},
		{
			name:    "required check with the reserved doorstep prefix",
			mutate:  func(c *Catalog) { c.Apps[0].Source.RequiredChecks = []string{"test", "doorstep/blog/before/checks"} },
			wantErr: []string{`source.required_checks[1]: "doorstep/blog/before/checks" uses the doorstep/ prefix that doorstep reserves for its own commit statuses`},
		},
		{
			name: "required check that only resembles the reserved prefix",
			mutate: func(c *Catalog) {
				c.Apps[0].Source.RequiredChecks = []string{"doorstep", "Doorstep/e2e", "ci/doorstep/e2e"}
			},
			wantErr: nil,
		},
		{
			name:    "image repository required",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "" },
			wantErr: []string{"image.repository: is required"},
		},
		{
			name:    "image repository without registry",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "example-user/blog" },
			wantErr: []string{"image.repository:", "must start with a registry host"},
		},
		{
			name:    "image repository single name",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "nginx" },
			wantErr: []string{"must start with a registry host"},
		},
		{
			name:    "image repository with tag",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "ghcr.io/example-user/blog:latest" },
			wantErr: []string{"image.repository:", "must not include a tag"},
		},
		{
			name:    "image repository with digest",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "ghcr.io/example-user/blog@sha256:abc" },
			wantErr: []string{"must not include a digest"},
		},
		{
			name:    "image repository uppercase path",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "ghcr.io/Example-User/blog" },
			wantErr: []string{"must have a lowercase path"},
		},
		{
			name:    "image repository empty path component",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "ghcr.io/example-user//blog" },
			wantErr: []string{"must have a lowercase path"},
		},
		{
			name:    "image repository host only",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "ghcr.io/" },
			wantErr: []string{"must have a lowercase path"},
		},
		{
			name:    "image repository bad port",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "registry:http/blog" },
			wantErr: []string{"must start with a registry host"},
		},
		{
			name:    "image repository empty port",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "registry:/blog" },
			wantErr: []string{"must start with a registry host"},
		},
		{
			name:    "image repository on localhost",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "localhost/blog" },
			wantErr: nil,
		},
		{
			name:    "image repository on host with port",
			mutate:  func(c *Catalog) { c.Apps[0].Image.Repository = "registry:5000/blog" },
			wantErr: nil,
		},
		{
			name:    "gitops repo required",
			mutate:  func(c *Catalog) { c.Apps[0].GitOps.Repo = "" },
			wantErr: []string{"gitops.repo: is required"},
		},
		{
			name:    "gitops branch invalid",
			mutate:  func(c *Catalog) { c.Apps[0].GitOps.Branch = "main.lock" },
			wantErr: []string{"gitops.branch:"},
		},
		{
			name:    "gitops path absolute",
			mutate:  func(c *Catalog) { c.Apps[0].GitOps.Path = "/apps/blog" },
			wantErr: []string{"apps[0] (blog): gitops.path: must be relative"},
		},
		{
			name:    "gitops path escapes the repository",
			mutate:  func(c *Catalog) { c.Apps[0].GitOps.Path = "../other" },
			wantErr: []string{"gitops.path: must not contain '..'"},
		},
		{
			name:    "gitops path root",
			mutate:  func(c *Catalog) { c.Apps[0].GitOps.Path = "" },
			wantErr: nil,
		},
		{
			name:    "no pin strategy",
			mutate:  func(c *Catalog) { c.Apps[0].Pin = Pin{} },
			wantErr: []string{"apps[0] (blog): pin: set exactly one of kustomize or yaml"},
		},
		{
			name: "both pin strategies",
			mutate: func(c *Catalog) {
				c.Apps[0].Pin.YAML = &YAMLPin{File: "values.yaml", Path: "a", Format: PinFormatDigest}
			},
			wantErr: []string{"pin: set exactly one of kustomize or yaml"},
		},
		{
			name:    "kustomize image empty",
			mutate:  func(c *Catalog) { c.Apps[0].Pin.Kustomize.Image = "" },
			wantErr: []string{"pin.kustomize.image: is required"},
		},
		{
			name:    "kustomize image with tag",
			mutate:  func(c *Catalog) { c.Apps[0].Pin.Kustomize.Image = "localhost:5000/blog:v1" },
			wantErr: []string{"pin.kustomize.image:", "must not include a tag"},
		},
		{
			name:    "kustomize image with digest",
			mutate:  func(c *Catalog) { c.Apps[0].Pin.Kustomize.Image = "blog@sha256:abc" },
			wantErr: []string{"pin.kustomize.image:", "must not include a digest"},
		},
		{
			name:    "kustomize image with whitespace",
			mutate:  func(c *Catalog) { c.Apps[0].Pin.Kustomize.Image = "my blog" },
			wantErr: []string{"must not contain whitespace"},
		},
		{
			name:    "kustomize short image name with registry port",
			mutate:  func(c *Catalog) { c.Apps[0].Pin.Kustomize.Image = "localhost:5000/blog" },
			wantErr: nil,
		},
		{
			name:    "yaml pin file required",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.File = "" },
			wantErr: []string{"apps[1] (api): pin.yaml.file: is required"},
		},
		{
			name:    "yaml pin file extension",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.File = "values.json" },
			wantErr: []string{`pin.yaml.file: "values.json" must end in .yaml or .yml`},
		},
		{
			name:    "yaml pin file absolute",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.File = "/values.yaml" },
			wantErr: []string{"pin.yaml.file: must be relative"},
		},
		{
			name:    "yaml pin file escapes",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.File = "../values.yml" },
			wantErr: []string{"pin.yaml.file: must not contain '..'"},
		},
		{
			name:    "yaml pin path required",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "" },
			wantErr: []string{"pin.yaml.path: is required"},
		},
		{
			name:    "yaml pin path empty segment",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "image..digest" },
			wantErr: []string{`pin.yaml.path: "image..digest" must be dot-separated keys`},
		},
		{
			name:    "yaml pin path trailing dot",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "image." },
			wantErr: []string{"pin.yaml.path:"},
		},
		{
			name:    "yaml pin path whitespace-only segment",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "image. " },
			wantErr: []string{`pin.yaml.path: "image. " must be dot-separated keys without empty segments or surrounding whitespace`},
		},
		{
			name:    "yaml pin path segment with leading space",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "image. digest" },
			wantErr: []string{"pin.yaml.path:", "surrounding whitespace"},
		},
		{
			name:    "yaml pin path segment with trailing tab",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "image\t.digest" },
			wantErr: []string{"pin.yaml.path:", "surrounding whitespace"},
		},
		{
			name:    "yaml pin path key with inner space",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "my image.digest" },
			wantErr: nil,
		},
		{
			name:    "yaml pin path with index",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Path = "spec.template.spec.containers.0.image" },
			wantErr: nil,
		},
		{
			name:    "yaml pin format unknown",
			mutate:  func(c *Catalog) { c.Apps[1].Pin.YAML.Format = "tag" },
			wantErr: []string{`pin.yaml.format: unknown format "tag"`},
		},
		{
			name:    "deploy mode unknown",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.Mode = "merge" },
			wantErr: []string{`deploy.mode: unknown mode "merge" (want commit or pull_request)`},
		},
		{
			name:    "deploy mode empty",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.Mode = "" },
			wantErr: []string{`deploy.mode: unknown mode ""`},
		},
		{
			name:    "max batch unset uses the default",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.MaxBatch = nil },
			wantErr: nil,
		},
		{
			name:    "max batch of one disables batching",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.MaxBatch = intPtr(1) },
			wantErr: nil,
		},
		{
			name:    "max batch at the upper bound",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.MaxBatch = intPtr(MaxMaxBatch) },
			wantErr: nil,
		},
		{
			name:    "max batch zero",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.MaxBatch = intPtr(0) },
			wantErr: []string{"deploy.max_batch: 0 is out of range (want 1 to 100)"},
		},
		{
			name:    "max batch negative",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.MaxBatch = intPtr(-3) },
			wantErr: []string{"deploy.max_batch: -3 is out of range"},
		},
		{
			name:    "max batch above the upper bound",
			mutate:  func(c *Catalog) { c.Apps[0].Deploy.MaxBatch = intPtr(MaxMaxBatch + 1) },
			wantErr: []string{"deploy.max_batch: 101 is out of range"},
		},
		{
			name:    "argocd application invalid",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.Application = "Blog_Prod" },
			wantErr: []string{"argocd.application:", "DNS subdomain"},
		},
		{
			name:    "argocd application with dots",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.Application = "blog.prod" },
			wantErr: nil,
		},
		{
			name:    "argocd application too long",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.Application = strings.Repeat("a.", 127) + "a" },
			wantErr: []string{"argocd.application: must be at most 253 characters"},
		},
		{
			name:    "argocd application required",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.Application = "" },
			wantErr: []string{"argocd.application: is required"},
		},
		{
			name:    "argocd namespace required",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.Namespace = "" },
			wantErr: []string{"argocd.namespace: is required"},
		},
		{
			name:    "argocd namespace invalid",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.Namespace = "argo.cd" },
			wantErr: []string{`argocd.namespace: "argo.cd" must be a DNS label`},
		},
		{
			name:   "argocd sync timeout",
			mutate: func(c *Catalog) { c.Apps[0].ArgoCD.SyncTimeout = durPtr(30 * time.Minute) },
		},
		{
			name:    "argocd sync timeout zero",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.SyncTimeout = durPtr(0) },
			wantErr: []string{"argocd.sync_timeout: must be positive"},
		},
		{
			name:    "argocd sync timeout negative",
			mutate:  func(c *Catalog) { c.Apps[0].ArgoCD.SyncTimeout = durPtr(-time.Minute) },
			wantErr: []string{"argocd.sync_timeout: must be positive"},
		},
		{
			name: "two apps share an Argo CD application",
			mutate: func(c *Catalog) {
				c.Apps[1].ArgoCD.Application = "blog"
			},
			wantErr: nil,
		},
		{
			name:    "gate type required",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.Before[0].Type = "" },
			wantErr: []string{"apps[0] (blog): gates.before[0].type: is required (want checks, external, health, lock, prometheus, schedule, soak)"},
		},
		{
			name:    "gate type unknown",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.Before[0].Type = "metric" },
			wantErr: []string{`gates.before[0].type: unknown type "metric"`},
		},
		{
			name: "checks gate after deploy",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.After = append(c.Apps[0].Gates.After, Gate{Type: GateChecks, Name: "ci", Timeout: durPtr(time.Minute)})
			},
			wantErr: []string{"gates.after[1].type: checks gates run only in gates.before"},
		},
		{
			name: "health gate before deploy",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, Gate{Type: GateHealth, Name: "early", Timeout: durPtr(time.Minute)})
			},
			wantErr: []string{"gates.before[1].type: health gates run only in gates.after"},
		},
		{
			name: "soak gate before deploy",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = []Gate{{Type: GateSoak, Name: "soak", Duration: Duration(time.Minute)}}
			},
			wantErr: []string{"gates.before[0].type: soak gates run only in gates.after"},
		},
		{
			name:    "gate timeout zero",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.Before[0].Timeout = durPtr(0) },
			wantErr: []string{"gates.before[0].timeout: must be positive"},
		},
		{
			name:    "gate timeout missing",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.Before[0].Timeout = nil },
			wantErr: []string{"gates.before[0].timeout: must be positive"},
		},
		{
			name:    "soak gate zero timeout",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[1].Timeout = durPtr(0) },
			wantErr: []string{"apps[1] (api): gates.after[1].timeout: must be positive"},
		},
		{
			name:    "gate timeout negative",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.After[0].Timeout = durPtr(-time.Second) },
			wantErr: []string{"gates.after[0].timeout: must be positive"},
		},
		{
			name:    "soak gate timeout shorter than duration",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[1].Timeout = durPtr(time.Minute) },
			wantErr: []string{"apps[1] (api): gates.after[1].timeout: 1m0s must be longer than duration 10m0s"},
		},
		{
			name:    "soak gate timeout equal to duration",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[1].Timeout = durPtr(10 * time.Minute) },
			wantErr: []string{"gates.after[1].timeout: 10m0s must be longer than duration 10m0s"},
		},
		{
			name:    "soak gate explicit timeout",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[1].Timeout = durPtr(time.Hour) },
			wantErr: nil,
		},
		{
			name:    "soak gate timeout missing",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[1].Timeout = nil },
			wantErr: []string{"gates.after[1].timeout: must be positive"},
		},
		{
			name: "lock gate",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, Gate{Type: GateLock, Name: "freeze"})
			},
			wantErr: nil,
		},
		{
			name: "lock gate after deploy",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.After = append(c.Apps[0].Gates.After, Gate{Type: GateLock, Name: "freeze"})
			},
			wantErr: []string{"gates.after[1].type: lock gates run only in gates.before"},
		},
		{
			name: "lock gate rejects timeout and duration",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, Gate{Type: GateLock, Name: "freeze", Timeout: durPtr(time.Hour), Duration: Duration(time.Minute)})
			},
			wantErr: []string{"gates.before[1].timeout: not allowed for type lock", "gates.before[1].duration: not allowed for type lock"},
		},
		{
			name: "lock gate rejects schedule fields",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, Gate{
					Type: GateLock, Name: "freeze", Timezone: "UTC", ApplyToRollbacks: true,
					Windows: []Window{{Effect: WindowDeny, Cron: "0 17 * * 5", Duration: Duration(time.Hour)}},
				})
			},
			wantErr: []string{
				"gates.before[1].windows: not allowed for type lock",
				"gates.before[1].timezone: not allowed for type lock",
				"gates.before[1].apply_to_rollbacks: not allowed for type lock",
			},
		},
		{
			name: "checks gate rejects an empty window list",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before[0].Windows = []Window{}
			},
			wantErr: []string{"gates.before[0].windows: not allowed for type checks"},
		},
		{
			name: "soak gate rejects schedule fields",
			mutate: func(c *Catalog) {
				c.Apps[1].Gates.After[1].Timezone = "Europe/London"
			},
			wantErr: []string{"gates.after[1].timezone: not allowed for type soak"},
		},
		{
			name: "external gate before deploy without timeout",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, Gate{Type: GateExternal, Name: "approval"})
			},
			wantErr: nil,
		},
		{
			name: "external gate after deploy with timeout and rollback opt-in",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.After = append(c.Apps[0].Gates.After, Gate{Type: GateExternal, Name: "e2e", Timeout: durPtr(30 * time.Minute), ApplyToRollbacks: true})
			},
			wantErr: nil,
		},
		{
			name: "external gate zero timeout",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.After = append(c.Apps[0].Gates.After, Gate{Type: GateExternal, Name: "e2e", Timeout: durPtr(0)})
			},
			wantErr: []string{"gates.after[1].timeout: must be positive"},
		},
		{
			name: "external gate rejects duration and windows",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.After = append(c.Apps[0].Gates.After, Gate{
					Type: GateExternal, Name: "e2e", Duration: Duration(time.Minute), Timezone: "UTC",
					Windows: []Window{{Effect: WindowDeny, Cron: "0 17 * * 5", Duration: Duration(time.Hour)}},
				})
			},
			wantErr: []string{
				"gates.after[1].duration: not allowed for type external",
				"gates.after[1].windows: not allowed for type external",
				"gates.after[1].timezone: not allowed for type external",
			},
		},
		{
			name: "external gates share the name space",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, Gate{Type: GateExternal, Name: "review"})
				c.Apps[0].Gates.After = append(c.Apps[0].Gates.After, Gate{Type: GateExternal, Name: "review"})
			},
			wantErr: []string{"gates.after[1].name: duplicate of gates.before[1]"},
		},
		{
			name:    "schedule gate",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, scheduleGate()) },
			wantErr: nil,
		},
		{
			name: "schedule gate after deploy",
			mutate: func(c *Catalog) {
				c.Apps[0].Gates.After = append(c.Apps[0].Gates.After, scheduleGate())
			},
			wantErr: []string{"gates.after[1].type: schedule gates run only in gates.before"},
		},
		{
			name: "schedule gate rejects timeout and duration",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Timeout, g.Duration = durPtr(time.Hour), Duration(time.Hour)
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{"gates.before[1].timeout: not allowed for type schedule", "gates.before[1].duration: not allowed for type schedule"},
		},
		{
			name: "schedule gate needs windows",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows = nil
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{"gates.before[1].windows: at least one window is required for type schedule"},
		},
		{
			name: "schedule gate with too many windows",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows = slices.Repeat(g.Windows[:1], MaxScheduleWindows+1)
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{"gates.before[1].windows: has 101 windows, more than the limit of 100"},
		},
		{
			name: "schedule gate bad time zone",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Timezone = "Mars/Base"
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{`gates.before[1].timezone: invalid time zone: "Mars/Base" is not an IANA time zone name`},
		},
		{
			name: "schedule gate Local time zone",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Timezone = "Local"
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{"gates.before[1].timezone: invalid time zone: Local depends on the host"},
		},
		{
			name: "schedule window errors are reported per field",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows = append(g.Windows, Window{Effect: "block", Cron: "0 0 17 * * 5", Duration: Duration(-time.Hour), Timezone: "Nowhere", Name: "bad\tname"})
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{
				`gates.before[1].windows[2].effect: unknown effect "block" (want allow or deny)`,
				`gates.before[1].windows[2].cron: invalid cron expression: "0 0 17 * * 5"`,
				"gates.before[1].windows[2].duration: invalid window duration: -1h0m0s must be positive",
				`gates.before[1].windows[2].timezone: invalid time zone: "Nowhere"`,
				"gates.before[1].windows[2].name: must not contain control characters",
			},
		},
		{
			name: "schedule window required fields",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows = []Window{{}}
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{
				"gates.before[1].windows[0].effect: is required (want allow or deny)",
				"gates.before[1].windows[0].cron: invalid cron expression: is empty",
				"gates.before[1].windows[0].duration: invalid window duration: 0s must be positive",
			},
		},
		{
			name: "schedule window rejects descriptors and time zone prefixes",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows[0].Cron = "@daily"
				g.Windows[1].Cron = "CRON_TZ=Europe/London 0 9 * * 1-5"
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{
				`gates.before[1].windows[0].cron: invalid cron expression: "@daily"`,
				"gates.before[1].windows[1].cron: invalid cron expression:", "must not set a time zone; use the timezone field",
			},
		},
		{
			name: "schedule window too long",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows[0].Duration = Duration(367 * 24 * time.Hour)
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{"gates.before[1].windows[0].duration: invalid window duration: 8808h0m0s is longer than 366 days"},
		},
		{
			name: "schedule window name too long",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows[0].Name = strings.Repeat("é", MaxWindowNameLength+1)
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{"gates.before[1].windows[0].name: must be at most 100 characters"},
		},
		{
			name: "schedule window name at length limit",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows[0].Name = strings.Repeat("é", MaxWindowNameLength)
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: nil,
		},
		{
			name: "schedule window name invalid UTF-8",
			mutate: func(c *Catalog) {
				g := scheduleGate()
				g.Windows[0].Name = "\xff"
				c.Apps[0].Gates.Before = append(c.Apps[0].Gates.Before, g)
			},
			wantErr: []string{"gates.before[1].windows[0].name: must be valid UTF-8"},
		},
		{
			name:    "soak gate duration required",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[1].Duration = 0 },
			wantErr: []string{"gates.after[1].duration: is required for type soak and must be positive"},
		},
		{
			name:    "soak gate duration negative",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[1].Duration = Duration(-time.Minute) },
			wantErr: []string{"gates.after[1].duration: is required for type soak"},
		},
		{
			name:    "health gate duration not allowed",
			mutate:  func(c *Catalog) { c.Apps[1].Gates.After[0].Duration = Duration(time.Minute) },
			wantErr: []string{"gates.after[0].duration: not allowed for type health"},
		},
		{
			name:    "gate name invalid",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.Before[0].Name = "CI Checks" },
			wantErr: []string{"gates.before[0].name:", "DNS label"},
		},
		{
			name:    "gate name empty",
			mutate:  func(c *Catalog) { c.Apps[0].Gates.Before[0].Name = "" },
			wantErr: []string{"gates.before[0].name: is required"},
		},
		{
			name: "gate names unique across phases",
			mutate: func(c *Catalog) {
				c.Apps[1].Gates.After[1].Name = "checks"
			},
			wantErr: []string{"apps[1] (api): gates.after[1].name: duplicate of gates.before[0]"},
		},
		{
			name: "two soak gates need distinct names",
			mutate: func(c *Catalog) {
				c.Apps[1].Gates.After = append(c.Apps[1].Gates.After, Gate{Type: GateSoak, Name: "soak", Duration: Duration(time.Hour)})
			},
			wantErr: []string{"gates.after[2].name: duplicate of gates.after[1]"},
		},
		{
			name:    "no gates at all",
			mutate:  func(c *Catalog) { c.Apps[0].Gates = Gates{} },
			wantErr: nil,
		},
		{
			name:    "notify unknown target",
			mutate:  func(c *Catalog) { c.Apps[0].Notify = []string{"pager"} },
			wantErr: []string{`apps[0] (blog): notify[0]: unknown notification "pager"`},
		},
		{
			name:    "notify duplicate target",
			mutate:  func(c *Catalog) { c.Apps[0].Notify = []string{"ops", "ops"} },
			wantErr: []string{`notify[1]: duplicate notification "ops"`},
		},
		{
			name: "duplicate kustomize pin",
			mutate: func(c *Catalog) {
				c.Apps[1].GitOps = c.Apps[0].GitOps
				c.Apps[1].Pin = Pin{Kustomize: &KustomizePin{Image: c.Apps[0].Pin.Kustomize.Image}}
			},
			wantErr: []string{"apps[1] (api): pin: writes the same image pin as apps[0] (blog) in example-user/homelab on branch main"},
		},
		{
			name: "duplicate pin with differently cased repository",
			mutate: func(c *Catalog) {
				c.Apps[1].GitOps = c.Apps[0].GitOps
				c.Apps[1].GitOps.Repo = "Example-User/HomeLab"
				c.Apps[1].Pin = Pin{Kustomize: &KustomizePin{Image: c.Apps[0].Pin.Kustomize.Image}}
			},
			wantErr: []string{"apps[1] (api): pin: writes the same image pin as apps[0] (blog)"},
		},
		{
			name: "duplicate yaml pin with equivalent path",
			mutate: func(c *Catalog) {
				c.Apps[0].GitOps = GitOps{Repo: "example-org/platform", Branch: "deploy", Path: "charts"}
				c.Apps[0].Pin = Pin{YAML: &YAMLPin{File: "api/values.yaml", Path: "image.digest", Format: PinFormatReference}}
			},
			wantErr: []string{"apps[1] (api): pin: writes the same image pin as apps[0] (blog)"},
		},
		{
			name: "yaml pin on a kustomization another app pins with kustomize",
			mutate: func(c *Catalog) {
				c.Apps[1].GitOps = GitOps{Repo: "Example-User/homelab", Branch: "main", Path: "apps"}
				c.Apps[1].Pin = Pin{YAML: &YAMLPin{File: "blog/kustomization.yaml", Path: "images.0.digest", Format: PinFormatDigest}}
			},
			wantErr: []string{"apps[1] (api): pin.yaml.file: writes the kustomization that apps[0] (blog) pins with kustomize; use pin.kustomize instead"},
		},
		{
			name: "yaml pin before the kustomize pin on the same kustomization",
			mutate: func(c *Catalog) {
				c.Apps[0].Pin = Pin{YAML: &YAMLPin{File: "kustomization.yml", Path: "images.0.digest", Format: PinFormatDigest}}
				c.Apps[1].GitOps = c.Apps[0].GitOps
				c.Apps[1].Pin = Pin{Kustomize: &KustomizePin{Image: "api"}}
			},
			wantErr: []string{"apps[0] (blog): pin.yaml.file: writes the kustomization that apps[1] (api) pins with kustomize"},
		},
		{
			name: "yaml pin on a kustomization at the repository root",
			mutate: func(c *Catalog) {
				c.Apps[0].GitOps.Path = ""
				c.Apps[1].GitOps = GitOps{Repo: "example-user/homelab", Branch: "main"}
				c.Apps[1].Pin = Pin{YAML: &YAMLPin{File: "kustomization.yaml", Path: "images.0.digest", Format: PinFormatDigest}}
			},
			wantErr: []string{"apps[1] (api): pin.yaml.file: writes the kustomization that apps[0] (blog)"},
		},
		{
			name: "yaml pin on a kustomization in another directory",
			mutate: func(c *Catalog) {
				c.Apps[1].GitOps = c.Apps[0].GitOps
				c.Apps[1].Pin = Pin{YAML: &YAMLPin{File: "overlay/kustomization.yaml", Path: "images.0.digest", Format: PinFormatDigest}}
			},
			wantErr: nil,
		},
		{
			name: "yaml pin on a kustomization on another branch",
			mutate: func(c *Catalog) {
				c.Apps[1].GitOps = c.Apps[0].GitOps
				c.Apps[1].GitOps.Branch = "staging"
				c.Apps[1].Pin = Pin{YAML: &YAMLPin{File: "kustomization.yaml", Path: "images.0.digest", Format: PinFormatDigest}}
			},
			wantErr: nil,
		},
		{
			name: "same kustomization with different images",
			mutate: func(c *Catalog) {
				c.Apps[1].GitOps = c.Apps[0].GitOps
				c.Apps[1].Pin = Pin{Kustomize: &KustomizePin{Image: "ghcr.io/example-user/sidecar"}}
			},
			wantErr: nil,
		},
		{
			name: "same file with different yaml paths",
			mutate: func(c *Catalog) {
				c.Apps[0].GitOps = c.Apps[1].GitOps
				c.Apps[0].Pin = Pin{YAML: &YAMLPin{File: "values.yaml", Path: "worker.image.digest", Format: PinFormatDigest}}
			},
			wantErr: nil,
		},
		{
			name: "same pin on different gitops branches",
			mutate: func(c *Catalog) {
				c.Apps[1].GitOps = c.Apps[0].GitOps
				c.Apps[1].GitOps.Branch = "staging"
				c.Apps[1].Pin = Pin{Kustomize: &KustomizePin{Image: c.Apps[0].Pin.Kustomize.Image}}
			},
			wantErr: nil,
		},
		{
			name:   "events for a slack target",
			mutate: func(c *Catalog) { c.Notifications[0].Events = []string{"deploy.failed", "deploy.rolled_back"} },
		},
		{
			name:   "every event for a webhook target",
			mutate: func(c *Catalog) { c.Notifications[1].Events = NotificationEvents() },
		},
		{
			name:    "events empty",
			mutate:  func(c *Catalog) { c.Notifications[0].Events = []string{} },
			wantErr: []string{"notifications[0] (ops): events: must not be empty; leave it out for the default events"},
		},
		{
			name:    "events unknown",
			mutate:  func(c *Catalog) { c.Notifications[1].Events = []string{"deploy.failed", "deploy_failed"} },
			wantErr: []string{`notifications[1] (hook): events[1]: unknown event "deploy_failed" (want one of deploy.queued,`},
		},
		{
			name: "events duplicate",
			mutate: func(c *Catalog) {
				c.Notifications[1].Events = []string{"lock.changed", "deploy.failed", "lock.changed"}
			},
			wantErr: []string{`notifications[1] (hook): events[2]: duplicate event "lock.changed"`},
		},
		{
			name:   "deployments disabled",
			mutate: func(c *Catalog) { c.Apps[0].Source.Deployments = new(false) },
		},
		{
			name: "every error is reported in file order",
			mutate: func(c *Catalog) {
				c.Notifications[0].Type = "email"
				c.Apps[0].GitOps.Path = "/abs"
				c.Apps[0].Deploy.Mode = "merge"
				c.Apps[1].Source.Repo = "nope"
				c.Apps[1].Notify = []string{"pager"}
			},
			wantErr: []string{
				"notifications[0] (ops): type",
				"apps[0] (blog): gitops.path",
				"apps[0] (blog): deploy.mode",
				"apps[1] (api): source.repo",
				"apps[1] (api): notify[0]",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := validCatalog()
			tt.mutate(&c)
			err := c.Validate()
			assertErrorContains(t, err, tt.wantErr)
			if err != nil {
				assertInOrder(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func assertInOrder(t *testing.T, s string, parts []string) {
	t.Helper()
	pos := 0
	for _, part := range parts {
		i := strings.Index(s[pos:], part)
		if i < 0 {
			t.Errorf("%q not found in order in %q", part, s)
			return
		}
		pos += i + len(part)
	}
}

func TestCatalogValidateLinesPerError(t *testing.T) {
	t.Parallel()

	c := validCatalog()
	c.Apps[0].Name = "Blog"
	c.Apps[0].Image.Repository = "blog"
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want errors")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 {
		t.Errorf("Validate() reported %d errors, want 2: %q", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "apps[0] (Blog): ") {
			t.Errorf("error %q is not prefixed with its location", line)
		}
	}
}

func TestCatalogLookups(t *testing.T) {
	t.Parallel()

	c := validCatalog()
	tests := []struct {
		name      string
		app       string
		wantApp   bool
		notify    string
		wantNotif bool
	}{
		{name: "both found", app: "api", wantApp: true, notify: "hook", wantNotif: true},
		{name: "neither found", app: "missing", notify: "missing"},
		{name: "names are case-sensitive", app: "Blog", notify: "OPS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app, ok := c.App(tt.app)
			if ok != tt.wantApp || (ok && app.Name != tt.app) {
				t.Errorf("App(%q) = %q, %t; want found %t", tt.app, app.Name, ok, tt.wantApp)
			}
			n, ok := c.Notification(tt.notify)
			if ok != tt.wantNotif || (ok && n.Name != tt.notify) {
				t.Errorf("Notification(%q) = %q, %t; want found %t", tt.notify, n.Name, ok, tt.wantNotif)
			}
		})
	}
}

func TestRepoName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		repo        RepoName
		owner, name string
	}{
		{repo: "example-user/blog", owner: "example-user", name: "blog"},
		{repo: "Org/My.Repo", owner: "Org", name: "My.Repo"},
		{repo: "noslash", owner: "noslash", name: ""},
		{repo: "", owner: "", name: ""},
	}

	for _, tt := range tests {
		t.Run(string(tt.repo), func(t *testing.T) {
			t.Parallel()
			if got := tt.repo.Owner(); got != tt.owner {
				t.Errorf("Owner() = %q, want %q", got, tt.owner)
			}
			if got := tt.repo.Name(); got != tt.name {
				t.Errorf("Name() = %q, want %q", got, tt.name)
			}
		})
	}
}

func TestPinStrategy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pin  Pin
		want PinStrategy
	}{
		{name: "kustomize", pin: Pin{Kustomize: &KustomizePin{}}, want: PinKustomize},
		{name: "yaml", pin: Pin{YAML: &YAMLPin{}}, want: PinYAML},
		{name: "none", pin: Pin{}, want: ""},
		{name: "both", pin: Pin{Kustomize: &KustomizePin{}, YAML: &YAMLPin{}}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.pin.Strategy(); got != tt.want {
				t.Errorf("Strategy() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestYAMLPinSegments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want []string
	}{
		{path: "", want: nil},
		{path: "image", want: []string{"image"}},
		{path: "image.digest", want: []string{"image", "digest"}},
		{path: "spec.template.spec.containers.0.image", want: []string{"spec", "template", "spec", "containers", "0", "image"}},
		{path: "a..b", want: []string{"a", "", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			got := YAMLPin{Path: tt.path}.Segments()
			if strings.Join(got, "|") != strings.Join(tt.want, "|") || len(got) != len(tt.want) {
				t.Errorf("Segments() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNotificationEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		n          Notification
		endpoint   SecretRef
		secret     SecretRef
		hasSecret  bool
		secretRefs int
	}{
		{
			name:       "slack env",
			n:          Notification{Name: "ops", Type: NotifySlack, WebhookURLEnv: "SLACK"},
			endpoint:   SecretRef{Label: "notification ops webhook_url", Env: "SLACK"},
			secret:     SecretRef{Label: "notification ops secret"},
			secretRefs: 1,
		},
		{
			name:       "slack file",
			n:          Notification{Name: "ops", Type: NotifySlack, WebhookURLFile: "/s"},
			endpoint:   SecretRef{Label: "notification ops webhook_url", File: "/s"},
			secret:     SecretRef{Label: "notification ops secret"},
			secretRefs: 1,
		},
		{
			name:       "webhook inline url with signing secret",
			n:          Notification{Name: "hook", Type: NotifyWebhook, URL: "https://x.example", SecretEnv: "SIG"},
			endpoint:   SecretRef{Label: "notification hook url", Value: "https://x.example"},
			secret:     SecretRef{Label: "notification hook secret", Env: "SIG"},
			hasSecret:  true,
			secretRefs: 1,
		},
		{
			name:       "webhook url file with secret file",
			n:          Notification{Name: "hook", Type: NotifyWebhook, URLFile: "/u", SecretFile: "/s"},
			endpoint:   SecretRef{Label: "notification hook url", File: "/u"},
			secret:     SecretRef{Label: "notification hook secret", File: "/s"},
			hasSecret:  true,
			secretRefs: 2,
		},
		{
			name:       "webhook inline url only",
			n:          Notification{Name: "hook", Type: NotifyWebhook, URL: "https://x.example"},
			endpoint:   SecretRef{Label: "notification hook url", Value: "https://x.example"},
			secret:     SecretRef{Label: "notification hook secret"},
			secretRefs: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.n.Endpoint(); got != tt.endpoint {
				t.Errorf("Endpoint() = %+v, want %+v", got, tt.endpoint)
			}
			secret, ok := tt.n.SigningSecret()
			if secret != tt.secret || ok != tt.hasSecret {
				t.Errorf("SigningSecret() = %+v, %t; want %+v, %t", secret, ok, tt.secret, tt.hasSecret)
			}
			refs := Catalog{Notifications: []Notification{tt.n}}.SecretRefs()
			if len(refs) != tt.secretRefs {
				t.Errorf("SecretRefs() = %+v, want %d refs", refs, tt.secretRefs)
			}
		})
	}
}

func TestNotificationEventNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []string
		want   []string
	}{
		{name: "omitted uses defaults", events: nil, want: DefaultNotificationEvents()},
		{name: "explicit list", events: []string{"deploy.failed"}, want: []string{"deploy.failed"}},
		{name: "empty list stays empty", events: []string{}, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := Notification{Events: tt.events}
			got := n.EventNames()
			if !slices.Equal(got, tt.want) {
				t.Errorf("EventNames() = %v, want %v", got, tt.want)
			}
			if len(got) > 0 {
				got[0] = "changed"
				if len(tt.events) > 0 && tt.events[0] == "changed" {
					t.Error("EventNames() shares memory with the configuration")
				}
			}
		})
	}
	for _, event := range DefaultNotificationEvents() {
		if !slices.Contains(NotificationEvents(), event) {
			t.Errorf("default event %q is not a known event", event)
		}
	}
}

func TestSourceStatusesEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *bool
		want bool
	}{
		{name: "omitted", in: nil, want: true},
		{name: "true", in: new(true), want: true},
		{name: "false", in: new(false), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (Source{Statuses: tt.in}).StatusesEnabled(); got != tt.want {
				t.Errorf("StatusesEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSourceDeploymentsEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *bool
		want bool
	}{
		{name: "omitted", in: nil, want: true},
		{name: "true", in: new(true), want: true},
		{name: "false", in: new(false), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := (Source{Deployments: tt.in}).DeploymentsEnabled(); got != tt.want {
				t.Errorf("DeploymentsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
