package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
)

func newAuthService(cfg config.Config, a config.Auth, repo auth.Repository, lookupEnv LookupEnv, logger *slog.Logger) (*auth.Service, error) {
	opts := auth.Options{Repository: repo, PublicURL: cfg.Server.PublicURL, SessionTTL: a.TTL(), Logger: logger}
	if !a.Enabled() {
		return auth.New(opts)
	}
	mapper, err := roleMapper(a)
	if err != nil {
		return nil, err
	}
	ref, _ := a.ClientSecret()
	secret := func() (string, error) { return ref.Read(lookupEnv, config.ReadSecretFile) }

	var provider auth.IdentityProvider
	switch a.Provider() {
	case config.ProviderOIDC:
		provider, err = auth.NewOIDCProvider(auth.OIDCOptions{
			IssuerURL:     a.OIDC.IssuerURL,
			ClientID:      a.OIDC.ClientID,
			ClientSecret:  secret,
			Scopes:        a.OIDC.Scopes,
			GroupsClaim:   a.OIDC.GroupsClaim,
			UsernameClaim: a.OIDC.UsernameClaim,
		})
	case config.ProviderGitHub:
		orgs, teams := mapper.GitHubLookups()
		provider, err = auth.NewGitHubProvider(auth.GitHubOptions{
			APIURL:       cfg.GitHub.APIURL,
			ClientID:     a.GitHub.ClientID,
			ClientSecret: secret,
			UserAgent:    "doorstep/" + buildinfo.Read().Version,
			Orgs:         orgs,
			Teams:        teams,
		})
	}
	if err != nil {
		return nil, err
	}
	opts.Provider = provider
	opts.Mapper = mapper
	return auth.New(opts)
}

func roleMapper(a config.Auth) (auth.RoleMapper, error) {
	m := auth.RoleMapper{Provider: a.Provider(), Bindings: make(map[core.Role]auth.Binding, len(a.Roles))}
	if a.DefaultRole != "" {
		role, err := core.ParseRole(a.DefaultRole)
		if err != nil {
			return auth.RoleMapper{}, fmt.Errorf("auth.default_role: %w", err)
		}
		m.Default = role
	}
	for name, binding := range a.Roles {
		role, err := core.ParseRole(name)
		if err != nil {
			return auth.RoleMapper{}, fmt.Errorf("auth.roles: %w", err)
		}
		m.Bindings[role] = auth.Binding{Groups: binding.Groups, Users: binding.Users}
	}
	return m, nil
}

func probeAuth(ctx context.Context, wg *sync.WaitGroup, logger *slog.Logger, svc *auth.Service) {
	name := svc.ProviderName()
	if name == "" {
		logger.InfoContext(ctx, "authentication configured", slog.String("provider", "tokens only"))
		return
	}
	logger.InfoContext(ctx, "authentication configured", slog.String("provider", name))
	wg.Go(func() {
		if err := svc.Probe(ctx); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "identity provider unreachable",
				slog.String("provider", name), slog.Any("error", err))
		}
	})
}
