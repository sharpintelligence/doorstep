package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultShutdownTimeout = 15 * time.Second
	checkTimeout           = 2 * time.Second

	statusOK           = "ok"
	statusUnavailable  = "unavailable"
	statusShuttingDown = "shutting_down"
)

type Check func(context.Context) error

type Options struct {
	Logger          *slog.Logger
	ShutdownTimeout time.Duration
	Checks          map[string]Check
}

type Server struct {
	logger          *slog.Logger
	shutdownTimeout time.Duration
	checks          map[string]Check
	shuttingDown    atomic.Bool
	handler         http.Handler
}

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func New(opts Options) *Server {
	s := &Server{
		logger:          opts.Logger,
		shutdownTimeout: opts.ShutdownTimeout,
		checks:          maps.Clone(opts.Checks),
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.shutdownTimeout <= 0 {
		s.shutdownTimeout = defaultShutdownTimeout
	}
	if s.checks == nil {
		s.checks = map[string]Check{}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.handler = logRequests(s.logger, recoverPanics(s.logger, mux))
	return s
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	s.shuttingDown.Store(true)
	s.logger.Info("shutting down", slog.Duration("timeout", s.shutdownTimeout))

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return errors.Join(fmt.Errorf("shutdown: %w", err), srv.Close())
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: statusOK})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: statusShuttingDown})
		return
	}

	results := s.runChecks(r.Context())
	resp := healthResponse{Status: statusOK, Checks: results}
	code := http.StatusOK
	for _, result := range results {
		if result != statusOK {
			resp.Status = statusUnavailable
			code = http.StatusServiceUnavailable
			break
		}
	}
	writeJSON(w, code, resp)
}

func (s *Server) runChecks(ctx context.Context) map[string]string {
	results := make(map[string]string, len(s.checks))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, check := range s.checks {
		wg.Go(func() {
			result := statusOK
			if err := runCheck(ctx, check); err != nil {
				result = err.Error()
				s.logger.WarnContext(ctx, "readiness check failed", slog.String("check", name), slog.Any("error", err))
			}
			mu.Lock()
			results[name] = result
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}

func runCheck(ctx context.Context, check Check) (err error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("check panicked: %v", v)
		}
	}()
	return check(ctx)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
