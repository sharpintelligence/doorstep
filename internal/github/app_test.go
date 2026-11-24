package github

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Parallel()

	key := generateRSAKey(t, 2048)
	smallKey := generateRSAKey(t, 1024)
	custom := &http.Client{Timeout: time.Second}

	tests := []struct {
		name    string
		opts    Options
		wantErr string
		wantIs  error
		check   func(t *testing.T, app *App)
	}{
		{
			name: "defaults",
			opts: Options{AppID: "1", PrivateKey: key},
			check: func(t *testing.T, app *App) {
				t.Helper()
				if got := app.baseURL.String(); got != DefaultBaseURL {
					t.Errorf("base URL = %q, want %q", got, DefaultBaseURL)
				}
				if app.httpClient.Timeout != 30*time.Second {
					t.Errorf("timeout = %s, want 30s", app.httpClient.Timeout)
				}
				if app.userAgent != "doorstep" {
					t.Errorf("user agent = %q", app.userAgent)
				}
				if app.logger == nil || app.now == nil {
					t.Error("logger or clock not defaulted")
				}
			},
		},
		{
			name: "custom options",
			opts: Options{
				AppID:      "Iv23liAbCdEf",
				PrivateKey: key,
				BaseURL:    "https://ghe.example.com/api/v3/",
				HTTPClient: custom,
				UserAgent:  "doorstep/v1.2.3",
			},
			check: func(t *testing.T, app *App) {
				t.Helper()
				if got := app.baseURL.String(); got != "https://ghe.example.com/api/v3" {
					t.Errorf("base URL = %q", got)
				}
				if app.httpClient != custom {
					t.Error("custom HTTP client not used")
				}
				if app.userAgent != "doorstep/v1.2.3" {
					t.Errorf("user agent = %q", app.userAgent)
				}
			},
		},
		{name: "missing app id", opts: Options{PrivateKey: key}, wantErr: "app id is required"},
		{name: "app id with whitespace", opts: Options{AppID: "12 34", PrivateKey: key}, wantErr: "must not contain whitespace"},
		{name: "missing key", opts: Options{AppID: "1"}, wantErr: "private key is required"},
		{name: "small key", opts: Options{AppID: "1", PrivateKey: smallKey}, wantIs: ErrInvalidPrivateKey},
		{
			name:   "plaintext remote base URL",
			opts:   Options{AppID: "1", PrivateKey: key, BaseURL: "http://ghe.example.com"},
			wantIs: ErrInvalidBaseURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app, err := New(tt.opts)
			if tt.wantErr == "" && tt.wantIs == nil {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				tt.check(t, app)
				return
			}
			if err == nil {
				t.Fatal("New succeeded, want error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    string
		wantErr string
	}{
		{raw: "https://api.github.com", want: "https://api.github.com"},
		{raw: "https://api.github.com/", want: "https://api.github.com"},
		{raw: "https://ghe.example.com/api/v3/", want: "https://ghe.example.com/api/v3"},
		{raw: "https://ghe.example.com:8443/api/v3", want: "https://ghe.example.com:8443/api/v3"},
		{raw: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{raw: "http://localhost:9000/api", want: "http://localhost:9000/api"},
		{raw: "http://[::1]:9000", want: "http://[::1]:9000"},
		{raw: "http://api.github.com", wantErr: "must use https unless the host is loopback"},
		{raw: "http://127.0.0.1.example.com", wantErr: "must use https unless the host is loopback"},
		{raw: "ftp://api.github.com", wantErr: `unsupported scheme "ftp"`},
		{raw: "api.github.com", wantErr: "absolute URL with a host"},
		{raw: "", wantErr: "absolute URL with a host"},
		{raw: "https://", wantErr: "absolute URL with a host"},
		{raw: "https:api.github.com", wantErr: "absolute URL with a host"},
		{raw: "https://x-access-token@api.github.com", wantErr: "must not contain user information"},
		{raw: "https://api.github.com?x=1", wantErr: "must not have a query"},
		{raw: "https://api.github.com?", wantErr: "must not have a query"},
		{raw: "https://api.github.com#frag", wantErr: "must not have a fragment"},
		{raw: "https://api.github.com/%zz", wantErr: "invalid URL escape"},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBaseURL(tt.raw)
			if tt.wantErr != "" {
				if !errors.Is(err, ErrInvalidBaseURL) {
					t.Fatalf("ParseBaseURL(%q) error = %v, want ErrInvalidBaseURL", tt.raw, err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "x-access-token") {
					t.Errorf("error %q leaks credentials", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBaseURL(%q): %v", tt.raw, err)
			}
			if got.String() != tt.want {
				t.Errorf("ParseBaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
