package cli

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/api"
	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/locks"
)

type recordedRequest struct {
	method      string
	path        string
	query       string
	auth        string
	contentType string
	body        string
}

type fakeAPI struct {
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	body     string
	header   map[string]string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		method:      r.Method,
		path:        r.URL.Path,
		query:       r.URL.RawQuery,
		auth:        r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		body:        string(body),
	})
	f.mu.Unlock()
	for k, v := range f.header {
		w.Header().Set(k, v)
	}
	if f.body != "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func (f *fakeAPI) only(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("server got %d requests, want 1: %+v", len(f.requests), f.requests)
	}
	return f.requests[0]
}

const (
	appLockJSON    = `{"scope":"app","app":"blog","owner":"alice","reason":"database migration","created_at":"2026-11-23T12:00:00Z","expires_at":"2026-11-23T14:00:00Z"}`
	globalLockJSON = `{"scope":"global","owner":"bob","reason":"holiday","created_at":"2026-11-23T10:00:00Z","expires_at":null}`
)

func runCLI(t *testing.T, args []string, env map[string]string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(t.Context(), args, &stdout, &stderr, envFrom(env))
	return code, stdout.String(), stderr.String()
}

func TestLockCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		status     int
		body       string
		wantMethod string
		wantPath   string
		wantBody   string
		wantStdout string
	}{
		{
			name:       "lock app",
			args:       []string{"lock", "blog", "--reason", "database migration"},
			status:     http.StatusCreated,
			body:       appLockJSON,
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/apps/blog/lock",
			wantBody:   `{"reason":"database migration"}`,
			wantStdout: "locked blog by alice until 2026-11-23T14:00:00Z: database migration\n",
		},
		{
			name:       "lock app for a duration",
			args:       []string{"lock", "blog", "--reason", "database migration", "--for", "90m"},
			status:     http.StatusCreated,
			body:       appLockJSON,
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/apps/blog/lock",
			wantBody:   `{"reason":"database migration","expires_in":"1h30m0s"}`,
			wantStdout: "locked blog by alice until 2026-11-23T14:00:00Z: database migration\n",
		},
		{
			name:       "lock globally until a time",
			args:       []string{"lock", "--global", "--reason", "holiday", "--until", "2027-01-02T10:00:00+01:00"},
			status:     http.StatusCreated,
			body:       globalLockJSON,
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/lock",
			wantBody:   `{"reason":"holiday","expires_at":"2027-01-02T10:00:00+01:00"}`,
			wantStdout: "locked global by bob until released: holiday\n",
		},
		{
			name:       "unlock app",
			args:       []string{"unlock", "blog"},
			status:     http.StatusNoContent,
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v1/apps/blog/lock",
			wantStdout: "unlocked blog\n",
		},
		{
			name:       "unlock global",
			args:       []string{"unlock", "--global"},
			status:     http.StatusNoContent,
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v1/lock",
			wantStdout: "unlocked global\n",
		},
		{
			name:       "list as table",
			args:       []string{"locks"},
			status:     http.StatusOK,
			body:       `{"locks":[` + globalLockJSON + `,` + appLockJSON + `]}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/locks",
			wantStdout: "SCOPE   OWNER  SINCE                 EXPIRES               REASON\n" +
				"global  bob    2026-11-23T10:00:00Z  -                     holiday\n" +
				"blog    alice  2026-11-23T12:00:00Z  2026-11-23T14:00:00Z  database migration\n",
		},
		{
			name:       "list replaces control characters",
			args:       []string{"locks"},
			status:     http.StatusOK,
			body:       `{"locks":[{"scope":"global","owner":"bo\u001bb","reason":"a\nb","created_at":"2026-11-23T10:00:00Z","expires_at":null}]}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/locks",
			wantStdout: "SCOPE   OWNER  SINCE                 EXPIRES  REASON\n" +
				"global  bo b   2026-11-23T10:00:00Z  -        a b\n",
		},
		{
			name:       "list empty",
			args:       []string{"locks"},
			status:     http.StatusOK,
			body:       `{"locks":[]}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/locks",
			wantStdout: "no locks\n",
		},
		{
			name:       "list as JSON",
			args:       []string{"locks", "-o", "json"},
			status:     http.StatusOK,
			body:       `{"locks":[` + globalLockJSON + `]}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/locks",
			wantStdout: "{\n  \"locks\": [\n    {\n      \"scope\": \"global\",\n      \"owner\": \"bob\",\n      \"reason\": \"holiday\",\n      \"created_at\": \"2026-11-23T10:00:00Z\",\n      \"expires_at\": null\n    }\n  ]\n}\n",
		},
		{
			name:       "list empty as JSON",
			args:       []string{"locks", "--output", "json"},
			status:     http.StatusOK,
			body:       `{}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/locks",
			wantStdout: "{\n  \"locks\": []\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: tt.status, body: tt.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			env := map[string]string{EnvServerURL: srv.URL + "/", EnvToken: "tok-123\n"}
			code, stdout, stderr := runCLI(t, tt.args, env)
			if code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr)
			}
			if stdout != tt.wantStdout {
				t.Errorf("stdout =\n%s\nwant\n%s", stdout, tt.wantStdout)
			}
			req := fake.only(t)
			if req.method != tt.wantMethod || req.path != tt.wantPath {
				t.Errorf("request = %s %s, want %s %s", req.method, req.path, tt.wantMethod, tt.wantPath)
			}
			if req.auth != "Bearer tok-123" {
				t.Errorf("Authorization = %q", req.auth)
			}
			if req.body != tt.wantBody {
				t.Errorf("body = %q, want %q", req.body, tt.wantBody)
			}
			if (tt.wantBody != "") != (req.contentType == "application/json") {
				t.Errorf("Content-Type = %q with body %q", req.contentType, req.body)
			}
		})
	}
}

func TestLockCommandErrors(t *testing.T) {
	t.Parallel()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	emptyFile := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(emptyFile, nil, 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	tests := []struct {
		name         string
		args         []string
		env          map[string]string
		status       int
		body         string
		header       map[string]string
		wantStderr   []string
		forbidStderr []string
		wantRequests int
	}{
		{name: "lock without scope", args: []string{"lock", "--reason", "r"}, wantStderr: []string{"an app name or --global is required"}},
		{name: "lock with app and global", args: []string{"lock", "blog", "--global", "--reason", "r"}, wantStderr: []string{"not both"}},
		{name: "lock without reason", args: []string{"lock", "blog"}, wantStderr: []string{`required flag(s) "reason" not set`}},
		{name: "lock with invalid app", args: []string{"lock", "..", "--reason", "r"}, wantStderr: []string{"app name", "DNS label"}},
		{name: "lock with two apps", args: []string{"lock", "a", "b", "--reason", "r"}, wantStderr: []string{"accepts at most 1 arg"}},
		{name: "lock for and until", args: []string{"lock", "blog", "--reason", "r", "--for", "1h", "--until", "2027-01-01T00:00:00Z"}, wantStderr: []string{"at most one of --for and --until"}},
		{name: "lock for zero", args: []string{"lock", "blog", "--reason", "r", "--for", "0s"}, wantStderr: []string{"--for: must be positive"}},
		{name: "lock until garbage", args: []string{"lock", "blog", "--reason", "r", "--until", "tomorrow"}, wantStderr: []string{"--until: must be an RFC 3339 time"}},
		{name: "unlock without scope", args: []string{"unlock"}, wantStderr: []string{"an app name or --global is required"}},
		{name: "locks bad output", args: []string{"locks", "-o", "yaml"}, wantStderr: []string{`--output: must be table or json, got "yaml"`}},
		{
			name:         "conflict shows the holder",
			args:         []string{"lock", "blog", "--reason", "r"},
			status:       http.StatusConflict,
			body:         `{"error":{"code":"locked","message":"blog is already locked by bob: migration"},"lock":` + appLockJSON + `}`,
			wantStderr:   []string{"doorstep: blog is already locked by bob: migration"},
			wantRequests: 1,
		},
		{
			name:         "not locked",
			args:         []string{"unlock", "blog"},
			status:       http.StatusNotFound,
			body:         `{"error":{"code":"not_found","message":"blog is not locked"}}`,
			wantStderr:   []string{"doorstep: blog is not locked"},
			wantRequests: 1,
		},
		{
			name:         "unauthorized without token hints at the variables",
			args:         []string{"locks"},
			env:          map[string]string{},
			status:       http.StatusUnauthorized,
			body:         `{"error":{"code":"unauthorized","message":"authentication required"}}`,
			wantStderr:   []string{"authentication required (set DOORSTEP_TOKEN or DOORSTEP_TOKEN_FILE)"},
			wantRequests: 1,
		},
		{
			name:         "non JSON error",
			args:         []string{"locks"},
			status:       http.StatusBadGateway,
			body:         "<html>bad gateway</html>",
			wantStderr:   []string{"server returned 502 Bad Gateway"},
			wantRequests: 1,
		},
		{
			name:         "redirect is not followed",
			args:         []string{"locks"},
			status:       http.StatusFound,
			header:       map[string]string{"Location": "https://elsewhere.example.com/api/v1/locks"},
			wantStderr:   []string{"redirect", "elsewhere.example.com"},
			wantRequests: 1,
		},
		{
			name:         "oversized response",
			args:         []string{"locks"},
			status:       http.StatusOK,
			body:         `{"locks":[],"pad":"` + strings.Repeat("x", maxResponseBytes) + `"}`,
			wantStderr:   []string{"larger than the 1048576 byte limit"},
			wantRequests: 1,
		},
		{
			name:         "malformed success body",
			args:         []string{"locks"},
			status:       http.StatusOK,
			body:         `{"locks":`,
			wantStderr:   []string{"decode response"},
			wantRequests: 1,
		},
		{name: "both token sources", args: []string{"locks"}, env: map[string]string{EnvToken: "a", EnvTokenFile: tokenFile}, wantStderr: []string{"at most one of DOORSTEP_TOKEN and DOORSTEP_TOKEN_FILE"}},
		{name: "empty token", args: []string{"locks"}, env: map[string]string{EnvToken: " "}, wantStderr: []string{"DOORSTEP_TOKEN: must not be empty"}},
		{name: "token with spaces", args: []string{"locks"}, env: map[string]string{EnvToken: "a b"}, wantStderr: []string{"printable ASCII"}, forbidStderr: []string{"a b"}},
		{name: "missing token file", args: []string{"locks"}, env: map[string]string{EnvTokenFile: "/nonexistent/token"}, wantStderr: []string{"DOORSTEP_TOKEN_FILE", "/nonexistent/token"}},
		{name: "empty token file", args: []string{"locks"}, env: map[string]string{EnvTokenFile: emptyFile}, wantStderr: []string{"DOORSTEP_TOKEN_FILE: file", "is empty"}},
		{name: "server flag with bad scheme", args: []string{"locks", "--server", "ftp://example.com"}, wantStderr: []string{"--server: scheme must be http or https"}},
		{name: "server env without host", args: []string{"locks"}, env: map[string]string{EnvServerURL: "http://"}, wantStderr: []string{"DOORSTEP_SERVER_URL: must include a host"}},
		{name: "server with credentials", args: []string{"locks", "--server", "https://u:secret@example.com"}, wantStderr: []string{"must not include user information"}, forbidStderr: []string{"secret"}},
		{name: "server with query", args: []string{"locks", "--server", "https://example.com/?token=x"}, wantStderr: []string{"must not include a query"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: tt.status, body: tt.body, header: tt.header}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			env := map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"}
			if tt.env != nil {
				env = map[string]string{EnvServerURL: srv.URL}
				maps.Copy(env, tt.env)
			}
			code, stdout, stderr := runCLI(t, tt.args, env)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stdout %q)", code, stdout)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not contain %q", stderr, want)
				}
			}
			for _, forbid := range tt.forbidStderr {
				if strings.Contains(stderr, forbid) {
					t.Errorf("stderr %q contains %q", stderr, forbid)
				}
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.requests) != tt.wantRequests {
				t.Errorf("server got %d requests, want %d", len(fake.requests), tt.wantRequests)
			}
		})
	}
}

func TestTokenFromFile(t *testing.T) {
	t.Parallel()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  file-token\r\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	fake := &fakeAPI{status: http.StatusOK, body: `{"locks":[]}`}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	code, _, stderr := runCLI(t, []string{"locks", "--server", srv.URL}, map[string]string{EnvTokenFile: tokenFile, EnvServerURL: "http://unused.invalid"})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr)
	}
	if got := fake.only(t).auth; got != "Bearer file-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestDefaultServerURL(t *testing.T) {
	t.Parallel()

	client, err := newAPIClient(func(string) bool { return false }, "", envFrom(nil))
	if err != nil {
		t.Fatalf("newAPIClient: %v", err)
	}
	if client.base.String() != defaultServerURL || client.token != "" {
		t.Errorf("client = %s, token %q", client.base, client.token)
	}
	if client.http.Timeout != apiTimeout {
		t.Errorf("timeout = %s", client.http.Timeout)
	}
}

func TestTokenRequiresTransportSecurity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		server  string
		token   string
		wantErr bool
	}{
		{name: "https", server: "https://doorstep.example.com", token: "t"},
		{name: "http loopback IPv4", server: "http://127.0.0.1:8080", token: "t"},
		{name: "http loopback IPv4 range", server: "http://127.1.2.3:8080", token: "t"},
		{name: "http loopback IPv6", server: "http://[::1]:8080", token: "t"},
		{name: "http localhost", server: "http://LocalHost:8080", token: "t"},
		{name: "http remote without token", server: "http://doorstep.internal:8080"},
		{name: "http remote host", server: "http://doorstep.internal:8080", token: "t", wantErr: true},
		{name: "http remote IP", server: "http://10.0.0.5", token: "t", wantErr: true},
		{name: "http localhost lookalike", server: "http://localhost.example.com", token: "t", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{EnvServerURL: tt.server}
			if tt.token != "" {
				env[EnvToken] = tt.token
			}
			_, err := newAPIClient(func(string) bool { return false }, "", envFrom(env))
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "plain http") {
					t.Errorf("newAPIClient(%s) = %v, want a plain http refusal", tt.server, err)
				}
				return
			}
			if err != nil {
				t.Errorf("newAPIClient(%s): %v", tt.server, err)
			}
		})
	}
}

func TestServerURLWithPathPrefix(t *testing.T) {
	t.Parallel()

	fake := &fakeAPI{status: http.StatusNoContent}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	code, _, stderr := runCLI(t, []string{"unlock", "blog"}, map[string]string{EnvServerURL: srv.URL + "/doorstep/", EnvToken: "t"})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr)
	}
	if got := fake.only(t).path; got != "/doorstep/api/v1/apps/blog/lock" {
		t.Errorf("path = %q", got)
	}
}

type staticAuth struct{}

func (staticAuth) Authenticate(r *http.Request, required core.Role) (api.Caller, error) {
	if r.Header.Get("Authorization") == "Bearer tok-123" {
		return api.Caller{Actor: core.Actor{Kind: core.ActorUser, ID: "alice"}, Role: required}, nil
	}
	return api.Caller{}, api.ErrUnauthenticated
}

type memoryLocks struct {
	mu    sync.Mutex
	locks map[string]core.Lock
}

func (m *memoryLocks) Lock(_ context.Context, req locks.Request) (core.Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.locks[core.LockRequest{Target: req.Target}.Scope()]; ok {
		return core.Lock{}, &core.LockHeldError{Lock: held}
	}
	l := core.Lock{Global: req.Target == nil, Kind: core.LockFreeze, Owner: req.Actor.String(), Reason: req.Reason}
	if req.Target != nil {
		l.Target = *req.Target
	}
	m.locks[core.LockRequest{Target: req.Target}.Scope()] = l
	return l, nil
}

func (m *memoryLocks) Unlock(_ context.Context, target *core.Target, _ core.Actor, _ core.Role) (core.Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scope := core.LockRequest{Target: target}.Scope()
	l, ok := m.locks[scope]
	if !ok {
		return core.Lock{}, core.ErrNotFound
	}
	delete(m.locks, scope)
	return l, nil
}

func (m *memoryLocks) List(context.Context) ([]core.Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.Lock
	if l, ok := m.locks[core.GlobalScope]; ok {
		out = append(out, l)
	}
	for scope, l := range m.locks {
		if scope != core.GlobalScope {
			out = append(out, l)
		}
	}
	return out, nil
}

func TestLockCommandsAgainstAPI(t *testing.T) {
	t.Parallel()

	handler := api.New(api.Options{
		Locks:         &memoryLocks{locks: map[string]core.Lock{}},
		KnownApp:      knownApp(oneAppCatalog(t)),
		Authenticator: staticAuth{},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	env := map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"}

	steps := []struct {
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{args: []string{"lock", "blog", "--reason", "migration"}, wantStdout: "locked blog by user:alice until released: migration\n"},
		{args: []string{"lock", "blog", "--reason", "again"}, wantCode: 1, wantStderr: "blog is already locked by user:alice: migration"},
		{args: []string{"lock", "other", "--reason", "x"}, wantCode: 1, wantStderr: "unknown app other"},
		{args: []string{"lock", "--global", "--reason", "holiday"}, wantStdout: "locked global by user:alice until released: holiday\n"},
		{args: []string{"locks", "-o", "json"}, wantStdout: `"scope": "global"`},
		{args: []string{"unlock", "blog"}, wantStdout: "unlocked blog\n"},
		{args: []string{"unlock", "blog"}, wantCode: 1, wantStderr: "blog is not locked"},
		{args: []string{"unlock", "--global"}, wantStdout: "unlocked global\n"},
		{args: []string{"locks"}, wantStdout: "no locks\n"},
	}
	for _, step := range steps {
		code, stdout, stderr := runCLI(t, step.args, env)
		if code != step.wantCode {
			t.Fatalf("%v: exit code = %d, want %d (stderr %q)", step.args, code, step.wantCode, stderr)
		}
		if !strings.Contains(stdout, step.wantStdout) {
			t.Errorf("%v: stdout %q does not contain %q", step.args, stdout, step.wantStdout)
		}
		if !strings.Contains(stderr, step.wantStderr) {
			t.Errorf("%v: stderr %q does not contain %q", step.args, stderr, step.wantStderr)
		}
	}
}

func TestKnownApp(t *testing.T) {
	t.Parallel()

	known := knownApp(oneAppCatalog(t))
	tests := []struct {
		app  string
		want bool
	}{
		{app: "blog", want: true},
		{app: "other", want: false},
		{app: "", want: false},
	}
	for _, tt := range tests {
		if got := known(tt.app); got != tt.want {
			t.Errorf("knownApp(%q) = %v, want %v", tt.app, got, tt.want)
		}
	}
}

func oneAppCatalog(t *testing.T) config.Catalog {
	t.Helper()
	file, err := loadFile(writeConfig(t, oneAppConfig))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return file.Catalog()
}
