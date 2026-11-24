package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func passing(context.Context) error { return nil }

func failing(msg string) Check {
	return func(context.Context) error { return errors.New(msg) }
}

func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		checks       map[string]Check
		shuttingDown bool
		method       string
		path         string
		wantCode     int
		wantBody     *healthResponse
		wantAllow    string
	}{
		{
			name:     "healthz",
			method:   http.MethodGet,
			path:     "/healthz",
			wantCode: http.StatusOK,
			wantBody: &healthResponse{Status: "ok"},
		},
		{
			name:     "healthz ignores failing checks",
			checks:   map[string]Check{"db": failing("down")},
			method:   http.MethodGet,
			path:     "/healthz",
			wantCode: http.StatusOK,
			wantBody: &healthResponse{Status: "ok"},
		},
		{
			name:         "healthz while shutting down",
			shuttingDown: true,
			method:       http.MethodGet,
			path:         "/healthz",
			wantCode:     http.StatusOK,
			wantBody:     &healthResponse{Status: "ok"},
		},
		{
			name:     "head healthz",
			method:   http.MethodHead,
			path:     "/healthz",
			wantCode: http.StatusOK,
		},
		{
			name:     "readyz without checks",
			method:   http.MethodGet,
			path:     "/readyz",
			wantCode: http.StatusOK,
			wantBody: &healthResponse{Status: "ok"},
		},
		{
			name:     "readyz all passing",
			checks:   map[string]Check{"db": passing, "cache": passing},
			method:   http.MethodGet,
			path:     "/readyz",
			wantCode: http.StatusOK,
			wantBody: &healthResponse{Status: "ok", Checks: map[string]string{"db": "ok", "cache": "ok"}},
		},
		{
			name:     "readyz one failing",
			checks:   map[string]Check{"db": failing("connection refused"), "cache": passing},
			method:   http.MethodGet,
			path:     "/readyz",
			wantCode: http.StatusServiceUnavailable,
			wantBody: &healthResponse{
				Status: "unavailable",
				Checks: map[string]string{"db": "fail", "cache": "ok"},
			},
		},
		{
			name: "readyz panicking check",
			checks: map[string]Check{"db": func(context.Context) error {
				panic("boom")
			}},
			method:   http.MethodGet,
			path:     "/readyz",
			wantCode: http.StatusServiceUnavailable,
			wantBody: &healthResponse{Status: "unavailable", Checks: map[string]string{"db": "fail"}},
		},
		{
			name:     "readyz failure text matching the pass label",
			checks:   map[string]Check{"db": failing("ok")},
			method:   http.MethodGet,
			path:     "/readyz",
			wantCode: http.StatusServiceUnavailable,
			wantBody: &healthResponse{Status: "unavailable", Checks: map[string]string{"db": "fail"}},
		},
		{
			name:         "readyz while shutting down",
			checks:       map[string]Check{"db": passing},
			shuttingDown: true,
			method:       http.MethodGet,
			path:         "/readyz",
			wantCode:     http.StatusServiceUnavailable,
			wantBody:     &healthResponse{Status: "shutting_down"},
		},
		{
			name:     "unknown path",
			method:   http.MethodGet,
			path:     "/nope",
			wantCode: http.StatusNotFound,
		},
		{
			name:      "wrong method",
			method:    http.MethodPost,
			path:      "/healthz",
			wantCode:  http.StatusMethodNotAllowed,
			wantAllow: "GET, HEAD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := New(Options{Checks: tt.checks})
			s.shuttingDown.Store(tt.shuttingDown)

			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantAllow != "" {
				if got := rec.Header().Get("Allow"); got != tt.wantAllow {
					t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
				}
			}
			if tt.wantBody == nil {
				return
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			assertHealthBody(t, rec.Body.Bytes(), *tt.wantBody)
		})
	}
}

func assertHealthBody(t *testing.T, body []byte, want healthResponse) {
	t.Helper()
	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal %q: %v", body, err)
	}
	if got.Status != want.Status {
		t.Errorf("status = %q, want %q", got.Status, want.Status)
	}
	if len(got.Checks) != len(want.Checks) {
		t.Fatalf("checks = %v, want %v", got.Checks, want.Checks)
	}
	for name, result := range want.Checks {
		if got.Checks[name] != result {
			t.Errorf("checks[%q] = %q, want %q", name, got.Checks[name], result)
		}
	}
}

func TestNewCopiesChecks(t *testing.T) {
	t.Parallel()

	checks := map[string]Check{"db": passing}
	s := New(Options{Checks: checks})
	checks["late"] = failing("added after New")

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	routes := map[string]http.Handler{
		"GET /metrics": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "custom") }),
		"GET /boom":    http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
	}
	tests := []struct {
		name     string
		routes   map[string]http.Handler
		method   string
		path     string
		wantCode int
		wantBody string
	}{
		{name: "mounted route served", routes: routes, method: http.MethodGet, path: "/metrics", wantCode: http.StatusOK, wantBody: "custom"},
		{name: "route method restricted", routes: routes, method: http.MethodPost, path: "/metrics", wantCode: http.StatusMethodNotAllowed},
		{name: "route not mounted without routes", method: http.MethodGet, path: "/metrics", wantCode: http.StatusNotFound},
		{name: "healthz still served", routes: routes, method: http.MethodGet, path: "/healthz", wantCode: http.StatusOK},
		{name: "readyz still served", routes: routes, method: http.MethodGet, path: "/readyz", wantCode: http.StatusOK},
		{name: "route panics are recovered", routes: routes, method: http.MethodGet, path: "/boom", wantCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := New(Options{Routes: tt.routes})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody))
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		timeout     time.Duration
		delay       time.Duration
		wantTimeout time.Duration
		wantDelay   time.Duration
	}{
		{name: "zero timeout uses default", wantTimeout: defaultShutdownTimeout},
		{name: "negative timeout uses default", timeout: -time.Second, wantTimeout: defaultShutdownTimeout},
		{name: "positive timeout kept", timeout: time.Second, wantTimeout: time.Second},
		{name: "negative delay clamped to zero", delay: -time.Second, wantTimeout: defaultShutdownTimeout},
		{name: "positive delay kept", delay: 3 * time.Second, wantTimeout: defaultShutdownTimeout, wantDelay: 3 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := New(Options{ShutdownTimeout: tt.timeout, ShutdownDelay: tt.delay})
			if s.shutdownTimeout != tt.wantTimeout {
				t.Errorf("shutdownTimeout = %s, want %s", s.shutdownTimeout, tt.wantTimeout)
			}
			if s.shutdownDelay != tt.wantDelay {
				t.Errorf("shutdownDelay = %s, want %s", s.shutdownDelay, tt.wantDelay)
			}
			if s.logger == nil {
				t.Error("logger is nil")
			}
		})
	}
}

func TestReadyzHidesCheckErrors(t *testing.T) {
	t.Parallel()

	const detail = "dial tcp db.internal:5432: connection refused"
	var logs lockedBuffer
	s := New(Options{
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		Checks: map[string]Check{"db": failing(detail)},
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))

	if strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("response leaks check error: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), detail) {
		t.Errorf("log %q does not contain check error", logs.String())
	}
}

func TestReadinessCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		offsets   []time.Duration
		wantCalls int
	}{
		{name: "single probe", offsets: []time.Duration{0}, wantCalls: 1},
		{name: "probes within ttl reuse result", offsets: []time.Duration{0, 500 * time.Millisecond, 999 * time.Millisecond}, wantCalls: 1},
		{name: "probe at ttl reruns checks", offsets: []time.Duration{0, checkCacheTTL}, wantCalls: 2},
		{
			name:      "each expiry reruns once",
			offsets:   []time.Duration{0, 200 * time.Millisecond, 1500 * time.Millisecond, 2 * time.Second, 2600 * time.Millisecond},
			wantCalls: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			s := New(Options{Checks: map[string]Check{"db": func(context.Context) error {
				calls.Add(1)
				return errors.New("down")
			}}})
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var offset time.Duration
			s.now = func() time.Time { return start.Add(offset) }

			for _, offset = range tt.offsets {
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("status at %s = %d, want %d", offset, rec.Code, http.StatusServiceUnavailable)
				}
			}
			if got := int(calls.Load()); got != tt.wantCalls {
				t.Errorf("check calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestReadinessConcurrentProbesShareOneRun(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	release := make(chan struct{})
	s := New(Options{Checks: map[string]Check{"db": func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	}}})

	const probes = 20
	var wg sync.WaitGroup
	codes := make(chan int, probes)
	for range probes {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", http.NoBody))
			codes <- rec.Code
		})
	}
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(codes)

	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("status = %d, want %d", code, http.StatusOK)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("check calls = %d, want 1", got)
	}
}

func TestReadinessSurvivesCancelledProbe(t *testing.T) {
	t.Parallel()

	s := New(Options{Checks: map[string]Check{"db": func(ctx context.Context) error { return ctx.Err() }}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRunCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		parent  func(t *testing.T) context.Context
		check   Check
		wantErr string
	}{
		{
			name:   "passes",
			parent: func(t *testing.T) context.Context { t.Helper(); return t.Context() },
			check:  passing,
		},
		{
			name:   "deadline bounded by check timeout",
			parent: func(t *testing.T) context.Context { t.Helper(); return t.Context() },
			check: func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				if !ok {
					return errors.New("no deadline")
				}
				if remaining := time.Until(deadline); remaining > checkTimeout {
					return errors.New("deadline too far away")
				}
				return nil
			},
		},
		{
			name: "observes parent cancellation",
			parent: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			check: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
			wantErr: context.Canceled.Error(),
		},
		{
			name:    "recovers panic",
			parent:  func(t *testing.T) context.Context { t.Helper(); return t.Context() },
			check:   func(context.Context) error { panic("kaboom") },
			wantErr: "check panicked: kaboom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := runCheck(tt.parent(t), tt.check)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("runCheck() = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("runCheck() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestRecoverPanics(t *testing.T) {
	t.Parallel()

	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := recoverPanics(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", http.NoBody))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	for _, want := range []string{`"msg":"handler panicked"`, `"panic":"handler exploded"`, `"path":"/boom"`, `"stack":`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not contain %s", logs.String(), want)
		}
	}
}

func TestRecoverPanicsRethrowsAbort(t *testing.T) {
	t.Parallel()

	h := recoverPanics(slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", err)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
}

func TestLogRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		handler   http.HandlerFunc
		minLevel  slog.Level
		wantLog   bool
		wantLevel string
		wantCode  float64
	}{
		{
			name:      "regular request at info",
			path:      "/api/thing",
			handler:   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) },
			minLevel:  slog.LevelInfo,
			wantLog:   true,
			wantLevel: "INFO",
			wantCode:  http.StatusCreated,
		},
		{
			name:      "implicit 200 on write",
			path:      "/api/thing",
			handler:   func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hi") },
			minLevel:  slog.LevelInfo,
			wantLog:   true,
			wantLevel: "INFO",
			wantCode:  http.StatusOK,
		},
		{
			name:      "implicit 200 without write",
			path:      "/api/thing",
			handler:   func(http.ResponseWriter, *http.Request) {},
			minLevel:  slog.LevelInfo,
			wantLog:   true,
			wantLevel: "INFO",
			wantCode:  http.StatusOK,
		},
		{
			name: "first status wins",
			path: "/api/thing",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot)
				w.WriteHeader(http.StatusInternalServerError)
			},
			minLevel:  slog.LevelInfo,
			wantLog:   true,
			wantLevel: "INFO",
			wantCode:  http.StatusTeapot,
		},
		{
			name:     "healthz hidden at info",
			path:     "/healthz",
			handler:  func(http.ResponseWriter, *http.Request) {},
			minLevel: slog.LevelInfo,
		},
		{
			name:     "readyz hidden at info",
			path:     "/readyz",
			handler:  func(http.ResponseWriter, *http.Request) {},
			minLevel: slog.LevelInfo,
		},
		{
			name:     "metrics hidden at info",
			path:     "/metrics",
			handler:  func(http.ResponseWriter, *http.Request) {},
			minLevel: slog.LevelInfo,
		},
		{
			name:      "metrics logged at debug",
			path:      "/metrics",
			handler:   func(http.ResponseWriter, *http.Request) {},
			minLevel:  slog.LevelDebug,
			wantLog:   true,
			wantLevel: "DEBUG",
			wantCode:  http.StatusOK,
		},
		{
			name:      "probe logged at debug",
			path:      "/healthz",
			handler:   func(http.ResponseWriter, *http.Request) {},
			minLevel:  slog.LevelDebug,
			wantLog:   true,
			wantLevel: "DEBUG",
			wantCode:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs lockedBuffer
			logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: tt.minLevel}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, http.NoBody)
			req.RemoteAddr = "192.0.2.1:1234"
			logRequests(logger, tt.handler).ServeHTTP(httptest.NewRecorder(), req)

			out := logs.String()
			if !tt.wantLog {
				if out != "" {
					t.Errorf("unexpected log %q", out)
				}
				return
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(out), &entry); err != nil {
				t.Fatalf("Unmarshal %q: %v", out, err)
			}
			want := map[string]any{
				"msg":    "request",
				"level":  tt.wantLevel,
				"method": http.MethodPost,
				"path":   tt.path,
				"status": tt.wantCode,
				"remote": "192.0.2.1:1234",
			}
			for key, value := range want {
				if entry[key] != value {
					t.Errorf("entry[%q] = %v, want %v", key, entry[key], value)
				}
			}
			if _, ok := entry["duration"]; !ok {
				t.Error("entry has no duration")
			}
		})
	}
}

func TestStatusRecorderUnwrap(t *testing.T) {
	t.Parallel()

	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush through recorder: %v", err)
	}
	if !inner.Flushed {
		t.Error("inner writer was not flushed")
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func TestRunGracefulShutdown(t *testing.T) {
	t.Parallel()

	ln := listen(t)
	s := New(Options{ShutdownTimeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()

	resp, err := get(t.Context(), "http://"+ln.Addr().String()+"/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if !s.shuttingDown.Load() {
		t.Error("server not marked as shutting down")
	}
}

func TestRunShutdownDelayReportsNotReady(t *testing.T) {
	t.Parallel()

	const delay = 2 * time.Second
	ln := listen(t)
	base := "http://" + ln.Addr().String()
	s := New(Options{ShutdownDelay: delay, ShutdownTimeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()

	assertProbe(t, base+"/readyz", http.StatusOK, statusOK)

	cancelled := time.Now()
	cancel()
	for !s.shuttingDown.Load() {
		time.Sleep(time.Millisecond)
	}

	assertProbe(t, base+"/readyz", http.StatusServiceUnavailable, statusShuttingDown)
	assertProbe(t, base+"/healthz", http.StatusOK, statusOK)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the shutdown delay")
	}
	if elapsed := time.Since(cancelled); elapsed < delay {
		t.Errorf("Run returned after %s, want at least %s", elapsed, delay)
	}
}

func assertProbe(t *testing.T, url string, wantCode int, wantStatus string) {
	t.Helper()
	resp, err := get(t.Context(), url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if resp.StatusCode != wantCode {
		t.Errorf("GET %s = %d, want %d (body %q)", url, resp.StatusCode, wantCode, body)
	}
	assertHealthBody(t, body, healthResponse{Status: wantStatus})
}

func TestRunCompletesInFlightRequest(t *testing.T) {
	t.Parallel()

	ln := listen(t)
	s := New(Options{ShutdownTimeout: 5 * time.Second})
	started := make(chan struct{})
	release := make(chan struct{})
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		if r.Context().Err() != nil {
			http.Error(w, "request context cancelled", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "finished")
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()

	type result struct {
		code int
		body string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		resp, err := get(t.Context(), "http://"+ln.Addr().String()+"/slow")
		if err != nil {
			results <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		results <- result{code: resp.StatusCode, body: string(body), err: err}
	}()

	<-started
	cancel()
	for !s.shuttingDown.Load() {
		time.Sleep(time.Millisecond)
	}
	close(release)

	res := <-results
	if res.err != nil {
		t.Fatalf("in-flight request failed: %v", res.err)
	}
	if res.code != http.StatusOK || res.body != "finished" {
		t.Errorf("in-flight response = %d %q, want 200 \"finished\"", res.code, res.body)
	}
	if err := <-done; err != nil {
		t.Errorf("Run() = %v, want nil", err)
	}
}

func TestRunShutdownTimeout(t *testing.T) {
	t.Parallel()

	ln := listen(t)
	s := New(Options{ShutdownTimeout: 50 * time.Millisecond})
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s.handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()
	go func() {
		resp, err := get(t.Context(), "http://"+ln.Addr().String()+"/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-started
	cancel()
	err := <-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() = %v, want deadline exceeded", err)
	}
}

func TestRunServeError(t *testing.T) {
	t.Parallel()

	ln := listen(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	err := New(Options{}).Run(t.Context(), ln)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Run() = %v, want net.ErrClosed", err)
	}
}

func TestRoutesRejectReservedPatterns(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	tests := []struct {
		name    string
		pattern string
		wantMsg string
	}{
		{name: "healthz", pattern: "GET /healthz", wantMsg: `"GET /healthz" is reserved`},
		{name: "readyz", pattern: "GET /readyz", wantMsg: `"GET /readyz" is reserved`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				v := recover()
				msg, _ := v.(string)
				if !strings.Contains(msg, tt.wantMsg) {
					t.Errorf("panic = %v, want message containing %q", v, tt.wantMsg)
				}
			}()
			New(Options{Routes: map[string]http.Handler{tt.pattern: handler}})
			t.Error("New did not panic")
		})
	}
}
