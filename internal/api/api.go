package api

import (
	"log/slog"
	"net/http"
)

const Prefix = "/api/v1/"

type Options struct {
	Logger     *slog.Logger
	Queue      QueueReader
	Apps       AppCatalog
	Middleware func(http.Handler) http.Handler
}

type api struct {
	logger *slog.Logger
	queue  QueueReader
	apps   AppCatalog
}

func New(opts Options) http.Handler {
	a := &api{logger: opts.Logger, queue: opts.Queue, apps: opts.Apps}
	if a.logger == nil {
		a.logger = slog.New(slog.DiscardHandler)
	}

	mux := http.NewServeMux()
	if a.queue != nil && a.apps != nil {
		a.registerQueue(mux)
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
