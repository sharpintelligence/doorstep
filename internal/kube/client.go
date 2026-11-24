package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	DefaultTimeout       = 30 * time.Second
	MaxResponseBytes     = 16 << 20
	MaxStreamBytes       = 64 << 20
	maxErrorBodyBytes    = 64 << 10
	maxErrorMessageBytes = 512
	defaultUserAgent     = "doorstep"
	dialTimeout          = 10 * time.Second
	idleConnTimeout      = 90 * time.Second
)

var (
	ErrForeignHost      = errors.New("request host does not match the kubernetes api server")
	ErrRedirect         = errors.New("kubernetes api redirects are not followed")
	ErrResponseTooLarge = errors.New("kubernetes api response is too large")
	ErrNotFound         = errors.New("kubernetes resource not found")
	ErrForbidden        = errors.New("kubernetes api access denied")
	ErrGone             = errors.New("kubernetes resource version is too old")
)

type Options struct {
	UserAgent string
	Timeout   time.Duration
}

type Client struct {
	server  *url.URL
	http    *http.Client
	timeout time.Duration
}

func New(cfg Config, opts Options) (*Client, error) {
	server, err := parseServer(cfg.Server)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.TLSServerName}
	if len(cfg.CAData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CAData) {
			return nil, fmt.Errorf("%w: certificate authority holds no PEM certificates", ErrInvalidConfig)
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.ClientCert != nil {
		tlsConfig.Certificates = []tls.Certificate{*cfg.ClientCert}
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	base := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       idleConnTimeout,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		server: server,
		http: &http.Client{
			Transport: &pinnedTransport{origin: server, token: cfg.Token, userAgent: userAgent, base: base},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return ErrRedirect
			},
		},
		timeout: timeout,
	}, nil
}

func (c *Client) Server() string {
	return c.server.String()
}

func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stream, err := c.send(ctx, path, query)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	body, err := io.ReadAll(io.LimitReader(stream, MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("kube api: read GET %s: %w", path, err)
	}
	if len(body) > MaxResponseBytes {
		return fmt.Errorf("kube api: GET %s: %w (over %d bytes)", path, ErrResponseTooLarge, MaxResponseBytes)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("kube api: decode GET %s: %w", path, err)
	}
	return nil
}

func (c *Client) Stream(ctx context.Context, path string, query url.Values) (io.ReadCloser, error) {
	stream, err := c.send(ctx, path, query)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(stream, MaxStreamBytes), stream}, nil
}

func (c *Client) send(ctx context.Context, path string, query url.Values) (io.ReadCloser, error) {
	u := c.server.JoinPath(path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("kube api: GET %s: %w", path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kube api: GET %s: %w", path, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, ParseStatus(resp.StatusCode, body)
	}
	return resp.Body, nil
}

type pinnedTransport struct {
	origin    *url.URL
	token     func() (string, error)
	userAgent string
	base      http.RoundTripper
}

func (t *pinnedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.origin.Scheme || req.URL.Host != t.origin.Host {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %s://%s", ErrForeignHost, req.URL.Scheme, req.URL.Host)
	}
	authed := req.Clone(req.Context())
	authed.Header.Set("Accept", "application/json")
	authed.Header.Set("User-Agent", t.userAgent)
	if t.token != nil {
		token, err := t.token()
		if err != nil {
			return nil, err
		}
		authed.Header.Set("Authorization", "Bearer "+token)
	}
	return t.base.RoundTrip(authed)
}

type StatusError struct {
	Code    int
	Reason  string
	Message string
}

func ParseStatus(code int, body []byte) *StatusError {
	var status struct {
		Code    int    `json:"code"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &status)
	if code == 0 {
		code = status.Code
	}
	return &StatusError{Code: code, Reason: status.Reason, Message: truncate(status.Message, maxErrorMessageBytes)}
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("kube api: %d %s", e.Code, http.StatusText(e.Code))
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Code == http.StatusNotFound
	case ErrForbidden:
		return e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden
	case ErrGone:
		return e.Code == http.StatusGone
	default:
		return false
	}
}

func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

func IsForbidden(err error) bool {
	return errors.Is(err, ErrForbidden)
}

func IsGone(err error) bool {
	return errors.Is(err, ErrGone)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	return s[:cut] + "…"
}
