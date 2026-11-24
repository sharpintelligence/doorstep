package auth

import (
	"context"
	"net/http"
	"time"
)

const providerTimeout = 30 * time.Second

type Identity struct {
	Subject       string
	Username      string
	Name          string
	Email         string
	EmailVerified bool
	Groups        []string
}

type IdentityProvider interface {
	Name() string
	AuthCodeURL(ctx context.Context, state, nonce, verifier, redirectURL string) (string, error)
	Exchange(ctx context.Context, code, nonce, verifier, redirectURL string) (Identity, error)
}

type prober interface {
	Probe(ctx context.Context) error
}

func defaultHTTPClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: providerTimeout}
}
