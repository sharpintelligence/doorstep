package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"golang.org/x/oauth2"

	"github.com/sharpintelligence/doorstep/internal/github"
)

const githubOrgScope = "read:org"

type GitHubOptions struct {
	APIURL       string
	ClientID     string
	ClientSecret func() (string, error)
	HTTPClient   *http.Client
	UserAgent    string
	Orgs         bool
	Teams        bool
}

type GitHubProvider struct {
	opts       GitHubOptions
	endpoint   oauth2.Endpoint
	httpClient *http.Client
}

func NewGitHubProvider(opts GitHubOptions) (*GitHubProvider, error) {
	if opts.ClientID == "" {
		return nil, errors.New("github sign-in: client id is required")
	}
	if opts.ClientSecret == nil {
		return nil, errors.New("github sign-in: client secret source is required")
	}
	authURL, tokenURL, err := github.OAuthEndpoints(opts.APIURL)
	if err != nil {
		return nil, fmt.Errorf("github sign-in: %w", err)
	}
	return &GitHubProvider{
		opts:       opts,
		endpoint:   oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams},
		httpClient: defaultHTTPClient(opts.HTTPClient),
	}, nil
}

func (p *GitHubProvider) Name() string {
	return ProviderGitHub
}

func (p *GitHubProvider) config(redirectURL string) oauth2.Config {
	return oauth2.Config{
		ClientID:    p.opts.ClientID,
		Endpoint:    p.endpoint,
		RedirectURL: redirectURL,
		Scopes:      []string{githubOrgScope},
	}
}

func (p *GitHubProvider) AuthCodeURL(_ context.Context, state, _, verifier, redirectURL string) (string, error) {
	cfg := p.config(redirectURL)
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

func (p *GitHubProvider) Exchange(ctx context.Context, code, _, verifier, redirectURL string) (Identity, error) {
	secret, err := p.opts.ClientSecret()
	if err != nil {
		return Identity{}, err
	}
	cfg := p.config(redirectURL)
	cfg.ClientSecret = secret
	token, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.httpClient), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("github sign-in: exchange code: %w", err)
	}
	client, err := github.NewUserClient(github.UserOptions{
		BaseURL:    p.opts.APIURL,
		Token:      token.AccessToken,
		HTTPClient: p.httpClient,
		UserAgent:  p.opts.UserAgent,
	})
	if err != nil {
		return Identity{}, err
	}
	user, err := client.User(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("github sign-in: %w", err)
	}
	var groups []string
	if p.opts.Orgs {
		orgs, err := client.OrgLogins(ctx)
		if err != nil {
			return Identity{}, fmt.Errorf("github sign-in: organization memberships: %w", err)
		}
		groups = append(groups, orgs...)
	}
	if p.opts.Teams {
		teams, err := client.TeamSlugs(ctx)
		if err != nil {
			return Identity{}, fmt.Errorf("github sign-in: team memberships: %w", err)
		}
		groups = append(groups, teams...)
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	return Identity{
		Subject:  strconv.FormatInt(user.ID, 10),
		Username: user.Login,
		Name:     name,
		Groups:   groups,
	}, nil
}
