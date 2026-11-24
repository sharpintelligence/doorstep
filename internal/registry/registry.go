package registry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

const (
	DefaultTimeout   = 30 * time.Second
	DefaultUserAgent = "doorstep"
)

type Digest string

var (
	ErrNotFound         = errors.New("image not found")
	ErrDenied           = errors.New("registry denied access")
	ErrInvalidReference = errors.New("invalid image reference")
)

type Options struct {
	Credentials CredentialSource
	Transport   http.RoundTripper
	UserAgent   string
	Timeout     time.Duration
}

type Resolver struct {
	credentials CredentialSource
	transport   http.RoundTripper
	userAgent   string
	timeout     time.Duration
}

func New(opts Options) *Resolver {
	r := &Resolver{
		credentials: opts.Credentials,
		transport:   opts.Transport,
		userAgent:   opts.UserAgent,
		timeout:     opts.Timeout,
	}
	if r.transport == nil {
		r.transport = defaultTransport()
	}
	if r.userAgent == "" {
		r.userAgent = DefaultUserAgent
	}
	if r.timeout <= 0 {
		r.timeout = DefaultTimeout
	}
	return r
}

func defaultTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

func (r *Resolver) Resolve(ctx context.Context, repository, tag string) (Digest, error) {
	ref, err := parseReference(repository, tag)
	if err != nil {
		return "", fmt.Errorf("resolve %s:%s: %w: %w", repository, tag, ErrInvalidReference, err)
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	auth, err := r.authenticator(ref.Context())
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref.Name(), err)
	}
	opts := r.remoteOptions(ctx, auth)

	head, err := remote.Head(ref, opts...)
	if err == nil {
		return Digest(head.Digest.String()), nil
	}
	if !fallsBackToGet(ctx, err) {
		return "", classify(ctx, ref, err)
	}

	got, err := remote.Get(ref, opts...)
	if err != nil {
		return "", classify(ctx, ref, err)
	}
	return Digest(got.Digest.String()), nil
}

func (r *Resolver) authenticator(repo name.Repository) (authn.Authenticator, error) {
	if r.credentials == nil {
		return authn.Anonymous, nil
	}
	cred, ok, err := r.credentials.Lookup(repo.RegistryStr(), repo.RepositoryStr())
	if err != nil {
		return nil, fmt.Errorf("look up registry credentials: %w", err)
	}
	if !ok {
		return authn.Anonymous, nil
	}
	return cred.authenticator(), nil
}

func (r *Resolver) remoteOptions(ctx context.Context, auth authn.Authenticator) []remote.Option {
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithTransport(r.transport),
		remote.WithAuth(auth),
		remote.WithUserAgent(r.userAgent),
		remote.WithRetryBackoff(remote.Backoff{Steps: 1}),
		remote.WithRetryPredicate(func(error) bool { return false }),
		remote.WithRetryStatusCodes(),
	}
}

func fallsBackToGet(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if status, ok := statusCode(err); ok {
		return !isTerminalStatus(status)
	}
	var netErr net.Error
	return !errors.As(err, &netErr)
}

func isTerminalStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		return true
	}
	return status >= http.StatusInternalServerError
}

func statusCode(err error) (int, bool) {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return 0, false
	}
	return terr.StatusCode, true
}

func classify(ctx context.Context, ref name.Tag, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = fmt.Errorf("%w: %w", ctxErr, err)
	}
	status, _ := statusCode(err)
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("resolve %s: %w: %w", ref.Name(), ErrNotFound, err)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("resolve %s: %w: %w", ref.Name(), ErrDenied, err)
	}
	return fmt.Errorf("resolve %s: %w", ref.Name(), err)
}
