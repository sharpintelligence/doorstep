package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/buildinfo"
	"github.com/sharpintelligence/doorstep/internal/config"
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

func envFrom(vars map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestRunVersion(t *testing.T) {
	t.Parallel()

	want := buildinfo.Read()
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, stdout string)
	}{
		{
			name: "text",
			args: []string{"version"},
			check: func(t *testing.T, stdout string) {
				t.Helper()
				if stdout != want.String()+"\n" {
					t.Errorf("stdout = %q, want %q", stdout, want.String()+"\n")
				}
			},
		},
		{
			name: "json",
			args: []string{"version", "--json"},
			check: func(t *testing.T, stdout string) {
				t.Helper()
				var got buildinfo.Info
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("Unmarshal %q: %v", stdout, err)
				}
				if got != want {
					t.Errorf("version JSON = %+v, want %+v", got, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := Run(t.Context(), tt.args, &stdout, &stderr, envFrom(nil))
			if code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("unexpected stderr %q", stderr.String())
			}
			tt.check(t, stdout.String())
		})
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantStderr []string
	}{
		{
			name:       "unknown command",
			args:       []string{"frobnicate"},
			wantStderr: []string{`doorstep: unknown command "frobnicate"`},
		},
		{
			name:       "unknown flag",
			args:       []string{"serve", "--nope"},
			wantStderr: []string{"doorstep: unknown flag: --nope"},
		},
		{
			name:       "bad duration flag",
			args:       []string{"serve", "--shutdown-timeout", "soon"},
			wantStderr: []string{"--shutdown-timeout", `"soon"`},
		},
		{
			name:       "unexpected argument",
			args:       []string{"version", "extra"},
			wantStderr: []string{"doorstep: unknown command \"extra\""},
		},
		{
			name: "invalid configuration reports every error",
			args: []string{"serve", "--listen-addr", "nowhere", "--log-level", "loud", "--log-format", "xml", "--shutdown-timeout", "0s"},
			wantStderr: []string{
				"doorstep: invalid configuration",
				"listen address",
				"shutdown timeout",
				"log level",
				"log format",
			},
		},
		{
			name:       "invalid environment",
			args:       []string{"serve"},
			env:        map[string]string{config.EnvShutdownTimeout: "later"},
			wantStderr: []string{"doorstep: invalid environment", config.EnvShutdownTimeout},
		},
		{
			name:       "invalid value from environment",
			args:       []string{"serve"},
			env:        map[string]string{config.EnvLogFormat: "xml"},
			wantStderr: []string{"doorstep: invalid configuration", "log format"},
		},
		{
			name:       "listen failure",
			args:       []string{"serve", "--listen-addr", "256.0.0.1:0"},
			wantStderr: []string{"doorstep: listen on 256.0.0.1:0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := Run(t.Context(), tt.args, &stdout, &stderr, envFrom(tt.env))
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stderr %q)", code, stderr.String())
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
			if strings.Contains(stderr.String(), "Usage:") {
				t.Errorf("stderr prints usage on error: %q", stderr.String())
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"serve", "--help"}, &stdout, &stderr, envFrom(nil)); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{"--listen-addr", "--log-level", "--log-format", "--shutdown-timeout", config.EnvListenAddr} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help %q does not mention %q", stdout.String(), want)
		}
	}
}

func TestResolveServeConfig(t *testing.T) {
	t.Parallel()

	flags := config.Config{
		Server: config.Server{ListenAddr: "127.0.0.1:7000", ShutdownTimeout: 3 * time.Second},
		Log:    config.Log{Level: "error", Format: "text"},
	}
	tests := []struct {
		name    string
		env     map[string]string
		changed []string
		want    func(*config.Config)
	}{
		{
			name: "defaults",
			want: func(*config.Config) {},
		},
		{
			name: "environment over defaults",
			env: map[string]string{
				config.EnvListenAddr:      ":9000",
				config.EnvShutdownTimeout: "1s",
				config.EnvLogLevel:        "debug",
				config.EnvLogFormat:       "text",
			},
			want: func(c *config.Config) {
				c.Server = config.Server{ListenAddr: ":9000", ShutdownTimeout: time.Second}
				c.Log = config.Log{Level: "debug", Format: "text"}
			},
		},
		{
			name:    "flags over defaults",
			changed: []string{flagListenAddr, flagShutdownTimeout, flagLogLevel, flagLogFormat},
			want:    func(c *config.Config) { *c = flags },
		},
		{
			name: "flags over environment",
			env: map[string]string{
				config.EnvListenAddr: ":9000",
				config.EnvLogLevel:   "debug",
			},
			changed: []string{flagListenAddr},
			want: func(c *config.Config) {
				c.Server.ListenAddr = flags.Server.ListenAddr
				c.Log.Level = "debug"
			},
		},
		{
			name:    "flag rescues invalid environment value",
			env:     map[string]string{config.EnvLogLevel: "loud"},
			changed: []string{flagLogLevel},
			want:    func(c *config.Config) { c.Log.Level = flags.Log.Level },
		},
		{
			name:    "unchanged flags ignored",
			env:     map[string]string{config.EnvLogFormat: "text"},
			changed: []string{flagShutdownTimeout},
			want: func(c *config.Config) {
				c.Server.ShutdownTimeout = flags.Server.ShutdownTimeout
				c.Log.Format = "text"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changed := func(name string) bool { return slices.Contains(tt.changed, name) }
			want := config.Default()
			tt.want(&want)

			got, err := resolveServeConfig(changed, flags, envFrom(tt.env))
			if err != nil {
				t.Fatalf("resolveServeConfig: %v", err)
			}
			if got != want {
				t.Errorf("resolveServeConfig() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRunServe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{
			name: "flags",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--log-level", "debug"},
		},
		{
			name: "environment",
			args: []string{"serve"},
			env:  map[string]string{config.EnvListenAddr: "127.0.0.1:0", config.EnvLogLevel: "debug"},
		},
		{
			name: "flag beats environment",
			args: []string{"serve", "--listen-addr", "127.0.0.1:0", "--log-format", "json"},
			env: map[string]string{
				config.EnvListenAddr: "invalid",
				config.EnvLogFormat:  "text",
				config.EnvLogLevel:   "debug",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			var stderr lockedBuffer
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan int, 1)
			go func() { done <- Run(ctx, tt.args, &stdout, &stderr, envFrom(tt.env)) }()

			addr := waitForListening(t, &stderr, done)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", http.NoBody)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /healthz: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
				t.Errorf("GET /healthz = %d %q", resp.StatusCode, body)
			}

			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("exit code = %d, stderr %q", code, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("serve did not exit after cancellation")
			}

			logs := parseLogs(t, stderr.String())
			for _, msg := range []string{"listening", "request", "shutting down", "stopped"} {
				if !hasMessage(logs, msg) {
					t.Errorf("logs missing %q: %s", msg, stderr.String())
				}
			}
			if stdout.Len() != 0 {
				t.Errorf("unexpected stdout %q", stdout.String())
			}
		})
	}
}

func waitForListening(t *testing.T, stderr *lockedBuffer, done <-chan int) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case code := <-done:
			t.Fatalf("serve exited early with %d: %s", code, stderr.String())
		default:
		}
		for _, entry := range parseLogs(t, stderr.String()) {
			if entry["msg"] != "listening" {
				continue
			}
			addr, ok := entry["addr"].(string)
			if !ok {
				t.Fatalf("listening log has no addr: %v", entry)
			}
			build, ok := entry["build"].(map[string]any)
			if !ok || build["version"] == "" {
				t.Errorf("listening log has no build info: %v", entry)
			}
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server never logged listening: %s", stderr.String())
	return ""
}

func parseLogs(t *testing.T, out string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", scanner.Text(), err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func hasMessage(entries []map[string]any, msg string) bool {
	for _, entry := range entries {
		if entry["msg"] == msg {
			return true
		}
	}
	return false
}
