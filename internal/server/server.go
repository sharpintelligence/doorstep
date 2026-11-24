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
	checkCacheTTL          = time.Second

	checkPassed = "ok"
	checkFailed = "fail"

	statusOK           = "ok"
	statusUnavailable  = "unavailable"
	statusShuttingDown = "shutting_down"
)

type Check func(context.Context) error

type Options struct {
	Logger          *slog.Logger
	ShutdownDelay   time.Duration
	ShutdownTimeout time.Duration
	Checks          map[string]Check
	Routes          map[string]http.Handler
}

type Server struct {
	logger          *slog.Logger
	shutdownDelay   time.Duration
	shutdownTimeout time.Duration
	checks          map[string]Check
	now             func() time.Time
	shuttingDown    atomic.Bool
	handler         http.Handler

	readinessMu   sync.Mutex
	readinessAt   time.Time
	lastReadiness readiness
}

type readiness struct {
	ready  bool
	checks map[string]string
}

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func New(opts Options) *Server {
	s := &Server{
		logger:          opts.Logger,
		shutdownDelay:   max(opts.ShutdownDelay, 0),
		shutdownTimeout: opts.ShutdownTimeout,
		checks:          maps.Clone(opts.Checks),
		now:             time.Now,
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
	for pattern, handler := range opts.Routes {
		mux.Handle(pattern, handler)
	}
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
	s.logger.InfoContext(ctx, "shutting down",
		slog.Duration("delay", s.shutdownDelay),
		slog.Duration("timeout", s.shutdownTimeout),
	)

	if s.shutdownDelay > 0 {
		delay := time.NewTimer(s.shutdownDelay)
		defer delay.Stop()
		select {
		case <-delay.C:
		case err := <-serveErr:
			return fmt.Errorf("serve: %w", err)
		}
	}

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

	result := s.readiness(r.Context())
	if !result.ready {
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: statusUnavailable, Checks: result.checks})
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: statusOK, Checks: result.checks})
}

func (s *Server) readiness(ctx context.Context) readiness {
	s.readinessMu.Lock()
	defer s.readinessMu.Unlock()
	if !s.readinessAt.IsZero() && s.now().Sub(s.readinessAt) < checkCacheTTL {
		return s.lastReadiness
	}
	s.lastReadiness = s.runChecks(context.WithoutCancel(ctx))
	s.readinessAt = s.now()
	return s.lastReadiness
}

func (s *Server) runChecks(ctx context.Context) readiness {
	result := readiness{ready: true, checks: make(map[string]string, len(s.checks))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, check := range s.checks {
		wg.Go(func() {
			err := runCheck(ctx, check)
			if err != nil {
				s.logger.WarnContext(ctx, "readiness check failed", slog.String("check", name), slog.Any("error", err))
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.ready = false
				result.checks[name] = checkFailed
				return
			}
			result.checks[name] = checkPassed
		})
	}
	wg.Wait()
	return result
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
