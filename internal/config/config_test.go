package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/logging"
)

func envFrom(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestDefault(t *testing.T) {
	t.Parallel()

	want := Config{
		Server: Server{ListenAddr: ":8080", ShutdownTimeout: 15 * time.Second},
		Log:    Log{Level: "info", Format: "json"},
	}
	got := Default()
	if got != want {
		t.Errorf("Default() = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		want    func(*Config)
		wantErr []string
	}{
		{
			name: "no variables keeps base",
			env:  map[string]string{},
			want: func(*Config) {},
		},
		{
			name: "listen address",
			env:  map[string]string{EnvListenAddr: "127.0.0.1:9000"},
			want: func(c *Config) { c.Server.ListenAddr = "127.0.0.1:9000" },
		},
		{
			name: "shutdown timeout",
			env:  map[string]string{EnvShutdownTimeout: "1m30s"},
			want: func(c *Config) { c.Server.ShutdownTimeout = 90 * time.Second },
		},
		{
			name: "log level",
			env:  map[string]string{EnvLogLevel: "debug"},
			want: func(c *Config) { c.Log.Level = "debug" },
		},
		{
			name: "log format",
			env:  map[string]string{EnvLogFormat: "text"},
			want: func(c *Config) { c.Log.Format = "text" },
		},
		{
			name: "empty value is applied and left to validation",
			env:  map[string]string{EnvLogLevel: ""},
			want: func(c *Config) { c.Log.Level = "" },
		},
		{
			name: "all variables",
			env: map[string]string{
				EnvListenAddr:      ":9090",
				EnvShutdownTimeout: "5s",
				EnvLogLevel:        "warn",
				EnvLogFormat:       "text",
			},
			want: func(c *Config) {
				c.Server = Server{ListenAddr: ":9090", ShutdownTimeout: 5 * time.Second}
				c.Log = Log{Level: "warn", Format: "text"}
			},
		},
		{
			name: "unparseable shutdown timeout keeps base value",
			env:  map[string]string{EnvShutdownTimeout: "soon", EnvLogLevel: "error"},
			want: func(c *Config) {
				c.Log.Level = "error"
			},
			wantErr: []string{EnvShutdownTimeout, `"soon"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := Default()
			tt.want(&want)

			got, err := FromEnv(Default(), envFrom(tt.env))
			assertErrorContains(t, err, tt.wantErr)
			if got != want {
				t.Errorf("FromEnv() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr []string
		wantIs  []error
	}{
		{name: "defaults", mutate: func(*Config) {}},
		{name: "host and port", mutate: func(c *Config) { c.Server.ListenAddr = "127.0.0.1:8080" }},
		{name: "ephemeral port", mutate: func(c *Config) { c.Server.ListenAddr = "127.0.0.1:0" }},
		{name: "ipv6", mutate: func(c *Config) { c.Server.ListenAddr = "[::1]:8080" }},
		{name: "text format", mutate: func(c *Config) { c.Log.Format = "text" }},
		{
			name:    "missing port",
			mutate:  func(c *Config) { c.Server.ListenAddr = "localhost" },
			wantErr: []string{"listen address", "missing port"},
		},
		{
			name:    "named port",
			mutate:  func(c *Config) { c.Server.ListenAddr = ":http" },
			wantErr: []string{"listen address", `port "http"`},
		},
		{
			name:    "port out of range",
			mutate:  func(c *Config) { c.Server.ListenAddr = ":70000" },
			wantErr: []string{"listen address", `port "70000"`},
		},
		{
			name:    "zero shutdown timeout",
			mutate:  func(c *Config) { c.Server.ShutdownTimeout = 0 },
			wantErr: []string{"shutdown timeout: must be positive, got 0s"},
		},
		{
			name:    "negative shutdown timeout",
			mutate:  func(c *Config) { c.Server.ShutdownTimeout = -time.Second },
			wantErr: []string{"shutdown timeout: must be positive, got -1s"},
		},
		{
			name:    "unknown log level",
			mutate:  func(c *Config) { c.Log.Level = "verbose" },
			wantErr: []string{"log level", `"verbose"`},
			wantIs:  []error{logging.ErrUnknownLevel},
		},
		{
			name:    "unknown log format",
			mutate:  func(c *Config) { c.Log.Format = "xml" },
			wantErr: []string{"log format", `"xml"`},
			wantIs:  []error{logging.ErrUnknownFormat},
		},
		{
			name: "every error is reported",
			mutate: func(c *Config) {
				c.Server = Server{ListenAddr: "nope", ShutdownTimeout: 0}
				c.Log = Log{Level: "loud", Format: "xml"}
			},
			wantErr: []string{"listen address", "shutdown timeout", "log level", "log format"},
			wantIs:  []error{logging.ErrUnknownLevel, logging.ErrUnknownFormat},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			assertErrorContains(t, err, tt.wantErr)
			for _, target := range tt.wantIs {
				if !errors.Is(err, target) {
					t.Errorf("Validate() error %v does not wrap %v", err, target)
				}
			}
		})
	}
}

func assertErrorContains(t *testing.T, err error, substrings []string) {
	t.Helper()
	if len(substrings) == 0 {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substrings)
	}
	for _, s := range substrings {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err, s)
		}
	}
}
