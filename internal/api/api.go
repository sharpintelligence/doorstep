package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

const Prefix = "/api/v1/"

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
)

type Authenticator interface {
	Authenticate(r *http.Request, required core.Role) (actor string, err error)
}

type RejectAll struct{}

func (RejectAll) Authenticate(*http.Request, core.Role) (string, error) {
	return "", ErrUnauthenticated
}

type LockService interface {
	Lock(ctx context.Context, req core.LockRequest) (core.Lock, error)
	Unlock(ctx context.Context, target *core.Target, actor string) (core.Lock, error)
	List(ctx context.Context) ([]core.Lock, error)
}

type KnownApp func(app string) bool

type History interface {
	ListDeployments(ctx context.Context, f store.DeploymentFilter) (store.DeploymentPage, error)
	GetDeploymentHistory(ctx context.Context, id core.DeploymentID) (store.DeploymentHistory, error)
}

type Options struct {
	Logger        *slog.Logger
	History       History
	Queue         QueueReader
	Apps          AppCatalog
	Locks         LockService
	KnownApp      KnownApp
	Authenticator Authenticator
	Middleware    func(http.Handler) http.Handler
}

type api struct {
	logger  *slog.Logger
	history History
	queue   QueueReader
	apps    AppCatalog
	locks   LockService
	known   KnownApp
	auth    Authenticator
}

func New(opts Options) http.Handler {
	a := &api{
		logger:  opts.Logger,
		history: opts.History,
		queue:   opts.Queue,
		apps:    opts.Apps,
		locks:   opts.Locks,
		known:   opts.KnownApp,
		auth:    opts.Authenticator,
	}
	if a.logger == nil {
		a.logger = slog.New(slog.DiscardHandler)
	}

	mux := http.NewServeMux()
	if a.history != nil {
		a.registerHistory(mux)
	}
	if a.queue != nil && a.apps != nil {
		a.registerQueue(mux)
	}
	if a.locks != nil && a.known != nil && a.auth != nil {
		a.registerLocks(mux)
	}
	mux.HandleFunc(Prefix, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "no such API endpoint")
	})

	var handler http.Handler = mux
	if opts.Middleware != nil {
		handler = opts.Middleware(handler)
	}
	return handler
}

func (a *api) authenticated(required core.Role, next func(w http.ResponseWriter, r *http.Request, actor string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := a.auth.Authenticate(r, required)
		if errors.Is(err, ErrForbidden) {
			writeError(w, http.StatusForbidden, codeForbidden, "your role does not allow this")
			return
		}
		if err != nil || actor == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="doorstep"`)
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "authentication required")
			return
		}
		next(w, r, actor)
	})
}
