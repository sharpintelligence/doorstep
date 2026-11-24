package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	LoginPath    = "/auth/login"
	CallbackPath = "/auth/callback"
	LogoutPath   = "/auth/logout"
)

type Options struct {
	Repository Repository
	PublicURL  string
	SessionTTL time.Duration
	Provider   IdentityProvider
	Mapper     RoleMapper
	Logger     *slog.Logger
	Now        func() time.Time
}

type Service struct {
	authenticator *Authenticator
	handler       *Handler
	tokens        *Tokens
	provider      IdentityProvider
}

func New(opts Options) (*Service, error) {
	if opts.Repository == nil {
		return nil, errors.New("auth: repository is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	tokens := NewTokens(opts.Repository, logger)
	a := &Authenticator{
		repo:        opts.Repository,
		tokens:      tokens,
		logger:      logger,
		crossOrigin: http.NewCrossOriginProtection(),
	}
	s := &Service{authenticator: a, tokens: tokens, provider: opts.Provider}

	var origin *url.URL
	if opts.PublicURL != "" {
		u, err := parsePublicURL(opts.PublicURL)
		if err != nil {
			return nil, err
		}
		if err := a.crossOrigin.AddTrustedOrigin(u.String()); err != nil {
			return nil, fmt.Errorf("auth: public url: %w", err)
		}
		origin = u
	}
	if opts.Provider == nil {
		return s, nil
	}

	var errs []error
	if origin == nil {
		errs = append(errs, errors.New("auth: a public url is required for sign-in"))
	}
	if opts.SessionTTL <= 0 {
		errs = append(errs, errors.New("auth: session ttl must be positive"))
	}
	if err := opts.Mapper.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("auth: role mapping: %w", err))
	}
	if opts.Mapper.Provider != opts.Provider.Name() {
		errs = append(errs, fmt.Errorf("auth: role mapping is for %q but the provider is %q", opts.Mapper.Provider, opts.Provider.Name()))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	c := newCookies(origin.Scheme == "https")
	a.cookies = c
	a.sessions = true
	s.handler = &Handler{
		repo:        opts.Repository,
		provider:    opts.Provider,
		mapper:      opts.Mapper,
		cookies:     c,
		redirectURL: origin.String() + CallbackPath,
		sessionTTL:  opts.SessionTTL,
		logger:      logger,
		now:         opts.Now,
	}
	if s.handler.now == nil {
		s.handler.now = time.Now
	}
	return s, nil
}

func (s *Service) Authenticator() *Authenticator {
	return s.authenticator
}

func (s *Service) Handler() *Handler {
	return s.handler
}

func (s *Service) Tokens() *Tokens {
	return s.tokens
}

func (s *Service) ProviderName() string {
	if s.provider == nil {
		return ""
	}
	return s.provider.Name()
}

func (s *Service) Probe(ctx context.Context) error {
	if p, ok := s.provider.(prober); ok {
		return p.Probe(ctx)
	}
	return nil
}

func parsePublicURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, fmt.Errorf("auth: public url %q must be an absolute http or https URL", raw)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("auth: public url %q must not have a path, query or fragment", raw)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}
