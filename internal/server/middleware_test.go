package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestOptionsMiddleware(t *testing.T) {
	t.Parallel()

	type middleware = func(http.Handler) http.Handler
	tag := func(name string) middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("X-Order", name)
				next.ServeHTTP(w, r)
			})
		}
	}
	deny := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Deny") != "" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	explode := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("middleware exploded") })
	}
	routes := map[string]http.Handler{
		"GET /hello": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hello") }),
	}

	tests := []struct {
		name       string
		middleware []middleware
		path       string
		deny       bool
		wantCode   int
		wantOrder  []string
		wantLogged string
	}{
		{name: "applied in order", middleware: []middleware{tag("first"), tag("second")}, path: "/hello", wantCode: http.StatusOK, wantOrder: []string{"first", "second"}},
		{name: "wraps built-in endpoints", middleware: []middleware{tag("only")}, path: "/healthz", wantCode: http.StatusOK, wantOrder: []string{"only"}},
		{name: "readyz still answers", middleware: []middleware{deny}, path: "/readyz", wantCode: http.StatusOK},
		{name: "can short circuit", middleware: []middleware{tag("first"), deny, tag("never")}, path: "/hello", deny: true, wantCode: http.StatusForbidden, wantOrder: []string{"first"}},
		{name: "panic is recovered and logged", middleware: []middleware{explode}, path: "/hello", wantCode: http.StatusInternalServerError, wantLogged: "middleware exploded"},
		{name: "no middleware", path: "/hello", wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs lockedBuffer
			s := New(Options{Routes: routes, Middleware: tt.middleware, Logger: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, http.NoBody)
			if tt.deny {
				req.Header.Set("X-Deny", "1")
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Header().Values("X-Order"); !slices.Equal(got, tt.wantOrder) {
				t.Errorf("middleware order = %v, want %v", got, tt.wantOrder)
			}
			if !strings.Contains(logs.String(), `"msg":"request"`) {
				t.Errorf("request not logged: %s", logs.String())
			}
			if tt.wantLogged != "" && !strings.Contains(logs.String(), tt.wantLogged) {
				t.Errorf("log %s does not contain %q", logs.String(), tt.wantLogged)
			}
		})
	}
}
