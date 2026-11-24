package config

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/logging"
)

const testDatabaseURL = "postgres://doorstep@localhost/doorstep"

func TestDefault(t *testing.T) {
	t.Parallel()

	want := Config{
		Server: Server{ListenAddr: ":8080", MetricsListenAddr: ":9090", ShutdownDelay: 5 * time.Second, ShutdownTimeout: 15 * time.Second},
		Log:    Log{Level: "info", Format: "json"},
		GitHub: GitHub{APIURL: "https://api.github.com"},
		Locks:  Locks{ReminderInterval: 24 * time.Hour},
	}
	got := Default()
	if got != want {
		t.Errorf("Default() = %+v, want %+v", got, want)
	}
	if err := got.Validate(); !errors.Is(err, ErrDatabaseSource) {
		t.Errorf("Default().Validate() = %v, want only ErrDatabaseSource", err)
	}
	got.Database.URL = testDatabaseURL
	if err := got.Validate(); err != nil {
		t.Errorf("Default() with a database URL: Validate() = %v", err)
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
			name: "metrics listen address",
			env:  map[string]string{EnvMetricsListenAddr: "127.0.0.1:9090"},
			want: func(c *Config) { c.Server.MetricsListenAddr = "127.0.0.1:9090" },
		},
		{
			name: "shutdown timeout",
			env:  map[string]string{EnvShutdownTimeout: "1m30s"},
			want: func(c *Config) { c.Server.ShutdownTimeout = 90 * time.Second },
		},
		{
			name: "shutdown delay",
			env:  map[string]string{EnvShutdownDelay: "0s"},
			want: func(c *Config) { c.Server.ShutdownDelay = 0 },
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
			name: "database URL",
			env:  map[string]string{EnvDatabaseURL: testDatabaseURL},
			want: func(c *Config) { c.Database.URL = testDatabaseURL },
		},
		{
			name: "database URL file",
			env:  map[string]string{EnvDatabaseURLFile: "/run/secrets/db"},
			want: func(c *Config) { c.Database.URLFile = "/run/secrets/db" },
		},
		{
			name: "both database sources are kept for validation",
			env:  map[string]string{EnvDatabaseURL: testDatabaseURL, EnvDatabaseURLFile: "/run/secrets/db"},
			want: func(c *Config) { c.Database = Database{URL: testDatabaseURL, URLFile: "/run/secrets/db"} },
		},
		{
			name: "registry credentials file",
			env:  map[string]string{EnvRegistryCredentialsFile: "/etc/doorstep/registry/.dockerconfigjson"},
			want: func(c *Config) { c.Registry.CredentialsFile = "/etc/doorstep/registry/.dockerconfigjson" },
		},
		{
			name: "lock reminder interval",
			env:  map[string]string{EnvLockReminderInterval: "12h"},
			want: func(c *Config) { c.Locks.ReminderInterval = 12 * time.Hour },
		},
		{
			name: "lock reminders disabled",
			env:  map[string]string{EnvLockReminderInterval: "0s"},
			want: func(c *Config) { c.Locks.ReminderInterval = 0 },
		},
		{
			name:    "unparseable lock reminder interval",
			env:     map[string]string{EnvLockReminderInterval: "daily"},
			want:    func(*Config) {},
			wantErr: []string{EnvLockReminderInterval, `"daily"`},
		},
		{
			name: "all variables",
			env: map[string]string{
				EnvListenAddr:        ":9090",
				EnvMetricsListenAddr: ":9091",
				EnvShutdownDelay:     "2s",
				EnvShutdownTimeout:   "5s",
				EnvLogLevel:          "warn",
				EnvLogFormat:         "text",
				EnvDatabaseURL:       testDatabaseURL,

				EnvRegistryCredentialsFile: "/creds.json",
			},
			want: func(c *Config) {
				c.Server = Server{ListenAddr: ":9090", MetricsListenAddr: ":9091", ShutdownDelay: 2 * time.Second, ShutdownTimeout: 5 * time.Second}
				c.Log = Log{Level: "warn", Format: "text"}
				c.Database.URL = testDatabaseURL
				c.Registry = Registry{CredentialsFile: "/creds.json"}
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
		{
			name:    "every unparseable duration is reported",
			env:     map[string]string{EnvShutdownDelay: "later", EnvShutdownTimeout: "soon"},
			want:    func(*Config) {},
			wantErr: []string{EnvShutdownDelay, `"later"`, EnvShutdownTimeout, `"soon"`},
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
		{name: "empty metrics listen address", mutate: func(c *Config) { c.Server.MetricsListenAddr = "" }},
		{name: "metrics host and port", mutate: func(c *Config) { c.Server.MetricsListenAddr = "127.0.0.1:9090" }},
		{name: "metrics ephemeral port", mutate: func(c *Config) { c.Server.MetricsListenAddr = ":0" }},
		{
			name:    "metrics missing port",
			mutate:  func(c *Config) { c.Server.MetricsListenAddr = "nope" },
			wantErr: []string{"metrics listen address", "missing port"},
		},
		{
			name:    "metrics named port",
			mutate:  func(c *Config) { c.Server.MetricsListenAddr = ":http" },
			wantErr: []string{"metrics listen address", `port "http"`},
		},
		{
			name: "metrics listen address equal to listen address",
			mutate: func(c *Config) {
				c.Server.ListenAddr = "127.0.0.1:8080"
				c.Server.MetricsListenAddr = "127.0.0.1:8080"
			},
			wantErr: []string{"metrics listen address", "must differ"},
		},
		{
			name: "metrics wildcard host clashes with listen address host",
			mutate: func(c *Config) {
				c.Server.ListenAddr = "127.0.0.1:8080"
				c.Server.MetricsListenAddr = ":8080"
			},
			wantErr: []string{"metrics listen address", "must differ"},
		},
		{
			name: "same port on different hosts",
			mutate: func(c *Config) {
				c.Server.ListenAddr = "127.0.0.1:8080"
				c.Server.MetricsListenAddr = "127.0.0.2:8080"
			},
		},
		{
			name: "both ephemeral ports",
			mutate: func(c *Config) {
				c.Server.ListenAddr = ":0"
				c.Server.MetricsListenAddr = ":0"
			},
		},
		{
			name:   "empty metrics address with any listen address",
			mutate: func(c *Config) { c.Server.MetricsListenAddr = "" },
		},
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
		{name: "zero shutdown delay", mutate: func(c *Config) { c.Server.ShutdownDelay = 0 }},
		{
			name:    "negative shutdown delay",
			mutate:  func(c *Config) { c.Server.ShutdownDelay = -time.Second },
			wantErr: []string{"shutdown delay: must not be negative, got -1s"},
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
		{name: "database URL file", mutate: func(c *Config) { c.Database = Database{URLFile: "/run/secrets/db"} }},
		{name: "lock reminders disabled", mutate: func(c *Config) { c.Locks.ReminderInterval = 0 }},
		{name: "lock reminder at minimum", mutate: func(c *Config) { c.Locks.ReminderInterval = time.Minute }},
		{
			name:    "lock reminder below minimum",
			mutate:  func(c *Config) { c.Locks.ReminderInterval = 59 * time.Second },
			wantErr: []string{"lock reminder interval: must be 0 (disabled) or at least 1m0s, got 59s"},
		},
		{
			name:    "negative lock reminder",
			mutate:  func(c *Config) { c.Locks.ReminderInterval = -time.Hour },
			wantErr: []string{"lock reminder interval: must not be negative, got -1h0m0s"},
		},
		{
			name:    "no database source",
			mutate:  func(c *Config) { c.Database = Database{} },
			wantErr: []string{"database", EnvDatabaseURL, EnvDatabaseURLFile},
			wantIs:  []error{ErrDatabaseSource},
		},
		{
			name:    "both database sources",
			mutate:  func(c *Config) { c.Database.URLFile = "/run/secrets/db" },
			wantErr: []string{"database", "exactly one"},
			wantIs:  []error{ErrDatabaseSource},
		},
		{
			name: "every error is reported",
			mutate: func(c *Config) {
				c.Server = Server{ListenAddr: "nope", ShutdownDelay: -time.Second, ShutdownTimeout: 0}
				c.Log = Log{Level: "loud", Format: "xml"}
				c.Database = Database{}
			},
			wantErr: []string{"listen address", "shutdown delay", "shutdown timeout", "log level", "log format", "database"},
			wantIs:  []error{logging.ErrUnknownLevel, logging.ErrUnknownFormat, ErrDatabaseSource},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Database.URL = testDatabaseURL
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

func TestDatabaseConnString(t *testing.T) {
	t.Parallel()

	errUnreadable := errors.New("permission denied")
	files := map[string]string{
		"/secrets/url":     testDatabaseURL,
		"/secrets/newline": testDatabaseURL + "\n",
		"/secrets/padded":  "  \t" + testDatabaseURL + " \r\n",
		"/secrets/empty":   "",
		"/secrets/blank":   " \n\t",
	}
	readFile := func(path string) ([]byte, error) {
		if path == "/secrets/unreadable" {
			return nil, &fs.PathError{Op: "open", Path: path, Err: errUnreadable}
		}
		content, ok := files[path]
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		return []byte(content), nil
	}

	tests := []struct {
		name    string
		db      Database
		want    string
		wantErr []string
		wantIs  error
	}{
		{name: "inline URL", db: Database{URL: testDatabaseURL}, want: testDatabaseURL},
		{name: "inline URL is not trimmed", db: Database{URL: " " + testDatabaseURL}, want: " " + testDatabaseURL},
		{name: "file", db: Database{URLFile: "/secrets/url"}, want: testDatabaseURL},
		{name: "trailing newline trimmed", db: Database{URLFile: "/secrets/newline"}, want: testDatabaseURL},
		{name: "surrounding whitespace trimmed", db: Database{URLFile: "/secrets/padded"}, want: testDatabaseURL},
		{name: "empty file", db: Database{URLFile: "/secrets/empty"}, wantErr: []string{"/secrets/empty", "empty"}},
		{name: "blank file", db: Database{URLFile: "/secrets/blank"}, wantErr: []string{"/secrets/blank", "empty"}},
		{name: "missing file", db: Database{URLFile: "/secrets/missing"}, wantErr: []string{"/secrets/missing"}, wantIs: fs.ErrNotExist},
		{name: "unreadable file", db: Database{URLFile: "/secrets/unreadable"}, wantErr: []string{"/secrets/unreadable", "permission denied"}, wantIs: errUnreadable},
		{name: "neither", db: Database{}, wantIs: ErrDatabaseSource},
		{name: "both", db: Database{URL: testDatabaseURL, URLFile: "/secrets/url"}, wantIs: ErrDatabaseSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.db.ConnString(readFile)
			if tt.wantErr == nil && tt.wantIs == nil {
				if err != nil {
					t.Fatalf("ConnString: %v", err)
				}
				if got != tt.want {
					t.Errorf("ConnString() = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if len(tt.wantErr) > 0 {
				assertErrorContains(t, err, tt.wantErr)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tt.wantIs)
			}
			if strings.Contains(err.Error(), "doorstep@localhost") {
				t.Errorf("error %q contains the connection string", err)
			}
			if got != "" {
				t.Errorf("ConnString() = %q on error", got)
			}
		})
	}
}

func TestFromEnvReplacesSecretSources(t *testing.T) {
	t.Parallel()

	base := Default()
	base.Database = Database{URLFile: "/file/database-url"}
	base.GitHub.AppID = "42"
	base.GitHub.PrivateKeyFile = "/file/key.pem"

	tests := []struct {
		name string
		env  map[string]string
		want func(*Config)
	}{
		{name: "unset keeps the base", want: func(*Config) {}},
		{
			name: "database URL replaces a URL file",
			env:  map[string]string{EnvDatabaseURL: testDatabaseURL},
			want: func(c *Config) { c.Database = Database{URL: testDatabaseURL} },
		},
		{
			name: "database URL file replaces a URL file",
			env:  map[string]string{EnvDatabaseURLFile: "/env/database-url"},
			want: func(c *Config) { c.Database = Database{URLFile: "/env/database-url"} },
		},
		{
			name: "empty database URL counts as set",
			env:  map[string]string{EnvDatabaseURL: ""},
			want: func(c *Config) { c.Database = Database{} },
		},
		{
			name: "both database sources conflict",
			env:  map[string]string{EnvDatabaseURL: testDatabaseURL, EnvDatabaseURLFile: "/env/database-url"},
			want: func(c *Config) { c.Database = Database{URL: testDatabaseURL, URLFile: "/env/database-url"} },
		},
		{
			name: "inline private key replaces a key file",
			env:  map[string]string{EnvGitHubAppPrivateKey: "inline"},
			want: func(c *Config) { c.GitHub.PrivateKey, c.GitHub.PrivateKeyFile = "inline", "" },
		},
		{
			name: "private key file replaces a key file",
			env:  map[string]string{EnvGitHubAppPrivateKeyFile: "/env/key.pem"},
			want: func(c *Config) { c.GitHub.PrivateKeyFile = "/env/key.pem" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := base
			tt.want(&want)
			got, err := FromEnv(base, envFrom(tt.env))
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if got != want {
				t.Errorf("FromEnv() = %+v, want %+v", got, want)
			}
		})
	}
}
