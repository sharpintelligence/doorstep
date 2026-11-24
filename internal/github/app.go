package github

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	tokenRefreshMargin   = 5 * time.Minute
	installationCacheTTL = time.Hour
	maxResponseBytes     = 10 << 20
	apiVersion           = "2022-11-28"
	mediaType            = "application/vnd.github+json"
	defaultTimeout       = 30 * time.Second
	defaultUserAgent     = "doorstep"
)

type Options struct {
	AppID      string
	PrivateKey *rsa.PrivateKey
	BaseURL    string
	HTTPClient *http.Client
	UserAgent  string
	Logger     *slog.Logger
	Now        func() time.Time
}

type App struct {
	appID      string
	key        *rsa.PrivateKey
	baseURL    *url.URL
	httpClient *http.Client
	userAgent  string
	logger     *slog.Logger
	now        func() time.Time

	mu            sync.Mutex
	installations map[string]installationEntry
	tokens        map[int64]*tokenEntry
}

func New(opts Options) (*App, error) {
	if opts.AppID == "" {
		return nil, errors.New("github: app id is required")
	}
	if strings.ContainsFunc(opts.AppID, unicode.IsSpace) {
		return nil, errors.New("github: app id must not contain whitespace")
	}
	if opts.PrivateKey == nil {
		return nil, errors.New("github: private key is required")
	}
	if err := checkKeySize(opts.PrivateKey); err != nil {
		return nil, err
	}
	rawBaseURL := opts.BaseURL
	if rawBaseURL == "" {
		rawBaseURL = DefaultBaseURL
	}
	baseURL, err := ParseBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}
	app := &App{
		appID:         opts.AppID,
		key:           opts.PrivateKey,
		baseURL:       baseURL,
		httpClient:    opts.HTTPClient,
		userAgent:     opts.UserAgent,
		logger:        opts.Logger,
		now:           opts.Now,
		installations: make(map[string]installationEntry),
		tokens:        make(map[int64]*tokenEntry),
	}
	if app.httpClient == nil {
		app.httpClient = &http.Client{Timeout: defaultTimeout}
	}
	if app.userAgent == "" {
		app.userAgent = defaultUserAgent
	}
	if app.logger == nil {
		app.logger = slog.New(slog.DiscardHandler)
	}
	if app.now == nil {
		app.now = time.Now
	}
	return app, nil
}

func (a *App) RepoClient(ctx context.Context, owner, repo string) (*Client, error) {
	if err := validateRepository(owner, repo); err != nil {
		return nil, err
	}
	id, err := a.installationID(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	base := a.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &Client{
		baseURL: a.baseURL,
		http: &http.Client{
			Transport:     &installationTransport{app: a, installationID: id, base: base},
			CheckRedirect: a.httpClient.CheckRedirect,
			Timeout:       a.httpClient.Timeout,
		},
	}, nil
}

func (a *App) appRequest(ctx context.Context, method, path string, out any) error {
	token, err := a.jwt()
	if err != nil {
		return err
	}
	req, err := newRequest(ctx, a.baseURL, method, path, nil)
	if err != nil {
		return err
	}
	a.setHeaders(req, token)
	return send(a.httpClient, req, out)
}

func (a *App) setHeaders(req *http.Request, bearer string) {
	req.Header.Set("Authorization", "Bearer "+bearer)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", mediaType)
	}
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", a.userAgent)
}
