package cli

import (
	"reflect"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/argocd"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gitops"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

func loadCatalog(t *testing.T, content string) config.Catalog {
	t.Helper()
	file, err := loadFile(writeConfig(t, content))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return file.Catalog()
}

func TestAppWiring(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		config      string
		wantIntake  intake.App
		wantGitOps  gitops.Target
		wantArgoCD  argocd.Application
		wantBatch   int
		wantTimeout time.Duration
	}{
		{
			name: "defaults",
			config: `
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
`,
			wantIntake: intake.App{
				Target:          core.AppTarget("blog"),
				Repository:      "example-user/blog",
				Branch:          "main",
				ImageRepository: "ghcr.io/example-user/blog",
			},
			wantGitOps: gitops.Target{
				App:             "blog",
				Owner:           "example-user",
				Repo:            "homelab",
				Branch:          "main",
				Dir:             "apps/blog",
				ImageRepository: "ghcr.io/example-user/blog",
				Kustomize:       &gitops.KustomizeImage{Name: "ghcr.io/example-user/blog"},
				Mode:            gitops.ModeCommit,
			},
			wantArgoCD: argocd.Application{
				Namespace:   "argocd",
				Name:        "blog",
				Repository:  "example-user/homelab",
				SyncTimeout: core.DefaultSyncTimeout,
			},
			wantBatch:   config.DefaultMaxBatch,
			wantTimeout: core.DefaultSyncTimeout,
		},
		{
			name: "kustomize image name, pull requests and overrides",
			config: `
apps:
  - name: shop
    source:
      repo: example-user/shop
      branch: release
      required_checks: [test, lint]
    image:
      repository: ghcr.io/example-user/shop
    gitops:
      repo: example-user/homelab
      branch: deploy
      path: apps/shop
    pin:
      kustomize:
        image: shop
    deploy:
      mode: pull_request
      max_batch: 3
    argocd:
      application: shop-prod
      namespace: gitops
      sync_timeout: 5m
`,
			wantIntake: intake.App{
				Target:          core.AppTarget("shop"),
				Repository:      "example-user/shop",
				Branch:          "release",
				RequiredChecks:  []string{"test", "lint"},
				ImageRepository: "ghcr.io/example-user/shop",
			},
			wantGitOps: gitops.Target{
				App:             "shop",
				Owner:           "example-user",
				Repo:            "homelab",
				Branch:          "deploy",
				Dir:             "apps/shop",
				ImageRepository: "ghcr.io/example-user/shop",
				Kustomize:       &gitops.KustomizeImage{Name: "shop"},
				Mode:            gitops.ModePullRequest,
			},
			wantArgoCD: argocd.Application{
				Namespace:   "gitops",
				Name:        "shop-prod",
				Repository:  "example-user/homelab",
				SyncTimeout: 5 * time.Minute,
			},
			wantBatch:   3,
			wantTimeout: 5 * time.Minute,
		},
		{
			name: "yaml reference",
			config: `
apps:
  - name: api
    source:
      repo: example-user/api
    image:
      repository: ghcr.io/example-user/api
    gitops:
      repo: example-user/homelab
      path: charts/api
    pin:
      yaml:
        file: values.yaml
        path: image.ref
        format: reference
`,
			wantIntake: intake.App{
				Target:          core.AppTarget("api"),
				Repository:      "example-user/api",
				Branch:          "main",
				ImageRepository: "ghcr.io/example-user/api",
			},
			wantGitOps: gitops.Target{
				App:             "api",
				Owner:           "example-user",
				Repo:            "homelab",
				Branch:          "main",
				Dir:             "charts/api",
				ImageRepository: "ghcr.io/example-user/api",
				YAML:            &gitops.YAMLValue{File: "values.yaml", Path: []string{"image", "ref"}, Format: gitops.FormatReference},
				Mode:            gitops.ModeCommit,
			},
			wantArgoCD: argocd.Application{
				Namespace:   "argocd",
				Name:        "api",
				Repository:  "example-user/homelab",
				SyncTimeout: core.DefaultSyncTimeout,
			},
			wantBatch:   config.DefaultMaxBatch,
			wantTimeout: core.DefaultSyncTimeout,
		},
		{
			name: "yaml digest",
			config: `
apps:
  - name: api
    source:
      repo: example-user/api
    image:
      repository: ghcr.io/example-user/api
    gitops:
      repo: example-user/homelab
      path: charts/api
    pin:
      yaml:
        file: values.yaml
        path: image.digest
`,
			wantIntake: intake.App{
				Target:          core.AppTarget("api"),
				Repository:      "example-user/api",
				Branch:          "main",
				ImageRepository: "ghcr.io/example-user/api",
			},
			wantGitOps: gitops.Target{
				App:             "api",
				Owner:           "example-user",
				Repo:            "homelab",
				Branch:          "main",
				Dir:             "charts/api",
				ImageRepository: "ghcr.io/example-user/api",
				YAML:            &gitops.YAMLValue{File: "values.yaml", Path: []string{"image", "digest"}, Format: gitops.FormatDigest},
				Mode:            gitops.ModeCommit,
			},
			wantArgoCD: argocd.Application{
				Namespace:   "argocd",
				Name:        "api",
				Repository:  "example-user/homelab",
				SyncTimeout: core.DefaultSyncTimeout,
			},
			wantBatch:   config.DefaultMaxBatch,
			wantTimeout: core.DefaultSyncTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			catalog := loadCatalog(t, tt.config)
			apps := newCatalogApps(catalog)
			w := apps[catalog.Apps[0].Name]
			target := tt.wantIntake.Target

			if got := w.target(); got != target {
				t.Errorf("target() = %v, want %v", got, target)
			}
			if got := w.intake(); !reflect.DeepEqual(got, tt.wantIntake) {
				t.Errorf("intake() = %+v, want %+v", got, tt.wantIntake)
			}
			got := w.gitops()
			if !reflect.DeepEqual(got, tt.wantGitOps) {
				t.Errorf("gitops() = %+v, want %+v", got, tt.wantGitOps)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("gitops() does not validate: %v", err)
			}
			if got := w.argocd(); got != tt.wantArgoCD {
				t.Errorf("argocd() = %+v, want %+v", got, tt.wantArgoCD)
			}
			if got := intakeApps(catalog); !reflect.DeepEqual(got, []intake.App{tt.wantIntake}) {
				t.Errorf("intakeApps() = %+v", got)
			}
			if got := syncApplications(catalog); !reflect.DeepEqual(got, map[core.Target]argocd.Application{target: tt.wantArgoCD}) {
				t.Errorf("syncApplications() = %+v", got)
			}
			if !apps.Known(target.App) || apps.Known("other") {
				t.Errorf("Known() does not match the catalog")
			}
			if got := apps.MaxBatch(target.App); got != tt.wantBatch {
				t.Errorf("MaxBatch() = %d, want %d", got, tt.wantBatch)
			}
			if got := apps.MaxBatch("other"); got != 1 {
				t.Errorf("MaxBatch(unknown) = %d, want 1", got)
			}
			if got, ok := apps.SyncTimeout(target); !ok || got != tt.wantTimeout {
				t.Errorf("SyncTimeout() = %s, %t, want %s", got, ok, tt.wantTimeout)
			}
			if _, ok := apps.SyncTimeout(core.AppTarget("other")); ok {
				t.Errorf("SyncTimeout(unknown) reports a configured app")
			}
		})
	}
}

func TestCatalogAppsAutoRollback(t *testing.T) {
	t.Parallel()

	catalog := loadCatalog(t, `
apps:
  - name: blog
    source:
      repo: example-user/blog
    image:
      repository: ghcr.io/example-user/blog
    gitops:
      repo: example-user/homelab
      path: apps/blog
    auto_rollback: true
  - name: shop
    source:
      repo: example-user/shop
    image:
      repository: ghcr.io/example-user/shop
    gitops:
      repo: example-user/homelab
      path: apps/shop
`)
	apps := newCatalogApps(catalog)
	tests := []struct {
		name   string
		target core.Target
		want   bool
	}{
		{name: "opted in", target: core.AppTarget("blog"), want: true},
		{name: "default is off", target: core.AppTarget("shop")},
		{name: "unknown app", target: core.AppTarget("wiki")},
		{name: "other environment", target: core.Target{App: "blog", Environment: "staging"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := apps.AutoRollback(tt.target); got != tt.want {
				t.Errorf("AutoRollback(%s) = %t, want %t", tt.target, got, tt.want)
			}
		})
	}
}
