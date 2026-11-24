package cli

import (
	"time"

	"github.com/sharpintelligence/doorstep/internal/argocd"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gitops"
	"github.com/sharpintelligence/doorstep/internal/intake"
)

type appWiring struct {
	app config.App
}

func (w appWiring) target() core.Target {
	return core.AppTarget(w.app.Name)
}

func (w appWiring) intake() intake.App {
	return intake.App{
		Target:          w.target(),
		Repository:      string(w.app.Source.Repo),
		Branch:          w.app.Source.Branch,
		RequiredChecks:  w.app.Source.RequiredChecks,
		ImageRepository: w.app.Image.Repository,
		Migrations:      migrationPolicy(w.app.Migrations),
	}
}

func migrationPolicy(m *config.Migrations) *intake.MigrationPolicy {
	if m == nil {
		return nil
	}
	return &intake.MigrationPolicy{Labels: m.Labels, Trailer: m.TrailerEnabled()}
}

func (w appWiring) gitops() gitops.Target {
	a := w.app
	t := gitops.Target{
		App:             a.Name,
		Owner:           a.GitOps.Repo.Owner(),
		Repo:            a.GitOps.Repo.Name(),
		Branch:          a.GitOps.Branch,
		Dir:             a.GitOps.Path,
		ImageRepository: a.Image.Repository,
		Mode:            gitops.ModeCommit,
	}
	if a.Deploy.Mode == config.DeployPullRequest {
		t.Mode = gitops.ModePullRequest
	}
	switch {
	case a.Pin.YAML != nil:
		format := gitops.FormatDigest
		if a.Pin.YAML.Format == config.PinFormatReference {
			format = gitops.FormatReference
		}
		t.YAML = &gitops.YAMLValue{File: a.Pin.YAML.File, Path: a.Pin.YAML.Segments(), Format: format}
	case a.Pin.Kustomize != nil && a.Pin.Kustomize.Image != "":
		t.Kustomize = &gitops.KustomizeImage{Name: a.Pin.Kustomize.Image}
	default:
		t.Kustomize = &gitops.KustomizeImage{Name: a.Image.Repository}
	}
	return t
}

func (w appWiring) argocd() argocd.Application {
	return argocd.Application{
		Namespace:   w.app.ArgoCD.Namespace,
		Name:        w.app.ArgoCD.Application,
		Repository:  string(w.app.GitOps.Repo),
		SyncTimeout: w.syncTimeout(),
	}
}

func (w appWiring) syncTimeout() time.Duration {
	return w.app.ArgoCD.Timeout()
}

type catalogApps map[string]appWiring

func newCatalogApps(catalog config.Catalog) catalogApps {
	apps := make(catalogApps, len(catalog.Apps))
	for _, a := range catalog.Apps {
		apps[a.Name] = appWiring{app: a}
	}
	return apps
}

func (c catalogApps) Known(app string) bool {
	_, ok := c[app]
	return ok
}

func (c catalogApps) MaxBatch(app string) int {
	a, ok := c[app]
	if !ok {
		return 1
	}
	return a.app.Deploy.BatchLimit()
}

func (c catalogApps) byTarget(t core.Target) (appWiring, bool) {
	a, ok := c[t.App]
	if !ok || a.target() != t {
		return appWiring{}, false
	}
	return a, true
}

func (c catalogApps) SyncTimeout(t core.Target) (time.Duration, bool) {
	a, ok := c.byTarget(t)
	if !ok {
		return 0, false
	}
	return a.syncTimeout(), true
}

func intakeApps(catalog config.Catalog) []intake.App {
	apps := make([]intake.App, 0, len(catalog.Apps))
	for _, a := range catalog.Apps {
		apps = append(apps, appWiring{app: a}.intake())
	}
	return apps
}

func syncApplications(catalog config.Catalog) map[core.Target]argocd.Application {
	apps := make(map[core.Target]argocd.Application, len(catalog.Apps))
	for _, a := range catalog.Apps {
		w := appWiring{app: a}
		apps[w.target()] = w.argocd()
	}
	return apps
}
