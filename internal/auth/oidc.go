package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

const (
	discoveryTimeout    = 10 * time.Second
	discoveryRetryAfter = 5 * time.Second
)

type OIDCOptions struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  func() (string, error)
	Scopes        []string
	GroupsClaim   string
	UsernameClaim string
	HTTPClient    *http.Client
	Now           func() time.Time
}

type OIDCProvider struct {
	opts       OIDCOptions
	httpClient *http.Client

	discovery singleflight.Group
	mu        sync.Mutex
	provider  *oidc.Provider
	failure   error
	failedAt  time.Time
}

func NewOIDCProvider(opts OIDCOptions) (*OIDCProvider, error) {
	var errs []error
	if opts.IssuerURL == "" {
		errs = append(errs, errors.New("oidc: issuer url is required"))
	}
	if opts.ClientID == "" {
		errs = append(errs, errors.New("oidc: client id is required"))
	}
	if opts.ClientSecret == nil {
		errs = append(errs, errors.New("oidc: client secret source is required"))
	}
	if opts.GroupsClaim == "" || opts.UsernameClaim == "" {
		errs = append(errs, errors.New("oidc: groups and username claims are required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &OIDCProvider{opts: opts, httpClient: defaultHTTPClient(opts.HTTPClient)}, nil
}

func (p *OIDCProvider) Name() string {
	return ProviderOIDC
}

func (p *OIDCProvider) Probe(ctx context.Context) error {
	_, err := p.discover(ctx)
	return err
}

func (p *OIDCProvider) discover(ctx context.Context) (*oidc.Provider, error) {
	if provider, ok, err := p.cachedDiscovery(); ok {
		return provider, err
	}
	result := p.discovery.DoChan("discover", func() (any, error) {
		if provider, ok, err := p.cachedDiscovery(); ok {
			return provider, err
		}
		return p.fetchDiscovery(context.WithoutCancel(ctx))
	})
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("oidc discovery for %s: %w", p.opts.IssuerURL, ctx.Err())
	case r := <-result:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*oidc.Provider), nil
	}
}

func (p *OIDCProvider) cachedDiscovery() (*oidc.Provider, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.provider != nil:
		return p.provider, true, nil
	case p.failure != nil && p.opts.Now().Sub(p.failedAt) < discoveryRetryAfter:
		return nil, true, p.failure
	}
	return nil, false, nil
}

func (p *OIDCProvider) fetchDiscovery(ctx context.Context) (*oidc.Provider, error) {
	ctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, p.httpClient), discoveryTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, p.opts.IssuerURL)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.failure = fmt.Errorf("oidc discovery for %s: %w", p.opts.IssuerURL, err)
		p.failedAt = p.opts.Now()
		return nil, p.failure
	}
	p.provider, p.failure = provider, nil
	return provider, nil
}

func (p *OIDCProvider) config(provider *oidc.Provider, redirectURL string) oauth2.Config {
	return oauth2.Config{
		ClientID:    p.opts.ClientID,
		Endpoint:    provider.Endpoint(),
		RedirectURL: redirectURL,
		Scopes:      p.opts.Scopes,
	}
}

func (p *OIDCProvider) AuthCodeURL(ctx context.Context, state, nonce, verifier, redirectURL string) (string, error) {
	provider, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	cfg := p.config(provider, redirectURL)
	return cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

func (p *OIDCProvider) Exchange(ctx context.Context, code, nonce, verifier, redirectURL string) (Identity, error) {
	provider, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	secret, err := p.opts.ClientSecret()
	if err != nil {
		return Identity{}, err
	}
	cfg := p.config(provider, redirectURL)
	cfg.ClientSecret = secret
	ctx = oidc.ClientContext(ctx, p.httpClient)

	token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return Identity{}, errors.New("oidc: token response has no id_token")
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: p.opts.ClientID, Now: p.opts.Now}).Verify(ctx, rawIDToken)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: verify id token: %w", err)
	}
	if !equalSecrets(idToken.Nonce, nonce) {
		return Identity{}, errors.New("oidc: id token nonce does not match the login")
	}
	claims := claimSet{}
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("oidc: decode id token claims: %w", err)
	}

	if provider.UserInfoEndpoint() != "" {
		info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
		if err != nil {
			return Identity{}, fmt.Errorf("oidc: userinfo: %w", err)
		}
		if info.Subject != idToken.Subject {
			return Identity{}, errors.New("oidc: userinfo subject does not match the id token")
		}
		extra := claimSet{}
		if err := info.Claims(&extra); err != nil {
			return Identity{}, fmt.Errorf("oidc: decode userinfo claims: %w", err)
		}
		for name, value := range extra {
			if _, ok := claims[name]; !ok {
				claims[name] = value
			}
		}
	}
	return p.identity(idToken.Subject, claims), nil
}

func (p *OIDCProvider) identity(subject string, claims claimSet) Identity {
	username := claims.str(p.opts.UsernameClaim)
	if strings.TrimSpace(username) == "" {
		username = subject
	}
	name := claims.str("name")
	if name == "" {
		name = username
	}
	return Identity{
		Subject:       subject,
		Username:      username,
		Name:          name,
		Email:         claims.str("email"),
		EmailVerified: claims.boolean("email_verified"),
		Groups:        claims.strings(p.opts.GroupsClaim),
	}
}

type claimSet map[string]json.RawMessage

func (c claimSet) str(name string) string {
	var s string
	_ = json.Unmarshal(c[name], &s)
	return s
}

func (c claimSet) boolean(name string) bool {
	var b bool
	if json.Unmarshal(c[name], &b) == nil {
		return b
	}
	return c.str(name) == "true"
}

func (c claimSet) strings(name string) []string {
	var list []string
	if json.Unmarshal(c[name], &list) == nil {
		return list
	}
	if s := c.str(name); s != "" {
		return []string{s}
	}
	return nil
}
