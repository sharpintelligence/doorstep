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
	appClient  *http.Client
	userAgent  string
	logger     *slog.Logger
	now        func() time.Time

	lookupFlights flightGroup[string, int64]
	tokenFlights  flightGroup[int64, string]

	mu            sync.Mutex
	installations map[string]installationEntry
	tokens        map[int64]tokenEntry
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
		lookupFlights: flightGroup[string, int64]{timeout: defaultTimeout},
		tokenFlights:  flightGroup[int64, string]{timeout: defaultTimeout},
		installations: make(map[string]installationEntry),
		tokens:        make(map[int64]tokenEntry),
	}
	if app.httpClient == nil {
		app.httpClient = &http.Client{Timeout: defaultTimeout}
	}
	app.appClient = app.pinnedClient(&pinnedTransport{origin: baseURL, base: app.baseTransport()})
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
	return &Client{
		baseURL: a.baseURL,
		http:    a.pinnedClient(&installationTransport{app: a, installationID: id, base: a.baseTransport()}),
	}, nil
}

func (a *App) baseTransport() http.RoundTripper {
	if a.httpClient.Transport != nil {
		return a.httpClient.Transport
	}
	return http.DefaultTransport
}

func (a *App) pinnedClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     transport,
		CheckRedirect: a.httpClient.CheckRedirect,
		Timeout:       a.httpClient.Timeout,
	}
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
	return send(a.appClient, req, out)
}

func (a *App) setHeaders(req *http.Request, bearer string) {
	req.Header.Set("Authorization", "Bearer "+bearer)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", mediaType)
	}
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", a.userAgent)
}
