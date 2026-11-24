package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sharpintelligence/doorstep/internal/core"
)

const Prefix = "/api/v1/"

var ErrUnauthenticated = errors.New("unauthenticated")

type Authenticator interface {
	Authenticate(r *http.Request) (actor string, err error)
}

type RejectAll struct{}

func (RejectAll) Authenticate(*http.Request) (string, error) {
	return "", ErrUnauthenticated
}

type LockService interface {
	Lock(ctx context.Context, req core.LockRequest) (core.Lock, error)
	Unlock(ctx context.Context, target *core.Target, actor string) (core.Lock, error)
	List(ctx context.Context) ([]core.Lock, error)
}

type KnownApp func(app string) bool

type Options struct {
	Logger        *slog.Logger
	Queue         QueueReader
	Apps          AppCatalog
	Locks         LockService
	KnownApp      KnownApp
	Authenticator Authenticator
	Middleware    func(http.Handler) http.Handler
}

type api struct {
	logger *slog.Logger
	queue  QueueReader
	apps   AppCatalog
	locks  LockService
	known  KnownApp
	auth   Authenticator
}

func New(opts Options) http.Handler {
	a := &api{
		logger: opts.Logger,
		queue:  opts.Queue,
		apps:   opts.Apps,
		locks:  opts.Locks,
		known:  opts.KnownApp,
		auth:   opts.Authenticator,
	}
	if a.logger == nil {
		a.logger = slog.New(slog.DiscardHandler)
	}

	mux := http.NewServeMux()
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

func (a *api) authenticated(next func(w http.ResponseWriter, r *http.Request, actor string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := a.auth.Authenticate(r)
		if err != nil || actor == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="doorstep"`)
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "authentication required")
			return
		}
		next(w, r, actor)
	})
}
