package cli

import (
	"log/slog"
	"net/http"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/web"
)

func webApps(catalog config.Catalog) []web.App {
	apps := make([]web.App, 0, len(catalog.Apps))
	for _, a := range catalog.Apps {
		app := web.App{
			Name:             a.Name,
			Target:           core.AppTarget(a.Name),
			Repository:       string(a.Source.Repo),
			Branch:           a.Source.Branch,
			GitOpsRepository: string(a.GitOps.Repo),
		}
		if a.ArgoCD.Application != "" {
			app.ArgoCDApplication = a.ArgoCD.Namespace + "/" + a.ArgoCD.Application
		}
		apps = append(apps, app)
	}
	return apps
}

type webInput struct {
	config        config.Config
	catalog       config.Catalog
	history       web.History
	queue         web.Queue
	locks         web.Locks
	lockEvents    web.LockEvents
	rollbacks     web.Rollbacks
	blocks        web.Blocks
	deploys       deployService
	audit         web.Audit
	tokens        web.Tokens
	authenticator *auth.Authenticator
	signIn        bool
	build         buildinfo.Info
	logger        *slog.Logger
}

func newWeb(in webInput) (map[string]http.Handler, error) {
	commitURL, err := githubCommitURL(in.config)
	if err != nil {
		return nil, err
	}
	opts := web.Options{
		Logger:       in.logger,
		Apps:         webApps(in.catalog),
		History:      in.history,
		Queue:        in.queue,
		Locks:        in.locks,
		LockEvents:   in.lockEvents,
		Rollbacks:    in.rollbacks,
		Blocks:       in.blocks,
		Deploys:      in.deploys,
		Audit:        in.audit,
		Tokens:       in.tokens,
		RequireLogin: in.authenticator.RequireRoleOrLogin(core.RoleViewer),
		SignIn:       in.signIn,
		CommitURL:    commitURL,
		Build:        in.build,
	}
	handler, err := web.New(opts)
	if err != nil {
		return nil, err
	}
	return handler.Routes(), nil
}
