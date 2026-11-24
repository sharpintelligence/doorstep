package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/github"
)

const fakeKey = "inline-key-material-supersecret"

func TestGitHubFromEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want GitHub
	}{
		{
			name: "unset keeps defaults",
			env:  map[string]string{},
			want: GitHub{APIURL: "https://api.github.com"},
		},
		{
			name: "key file",
			env: map[string]string{
				EnvGitHubAppID:             "123456",
				EnvGitHubAppPrivateKeyFile: "/var/run/secrets/github/private-key.pem",
			},
			want: GitHub{AppID: "123456", PrivateKeyFile: "/var/run/secrets/github/private-key.pem", APIURL: "https://api.github.com"},
		},
		{
			name: "inline key and enterprise url",
			env: map[string]string{
				EnvGitHubAppID:         "Iv23liAbCdEf",
				EnvGitHubAppPrivateKey: fakeKey,
				EnvGitHubAPIURL:        "https://ghe.example.com/api/v3",
			},
			want: GitHub{AppID: "Iv23liAbCdEf", PrivateKey: fakeKey, APIURL: "https://ghe.example.com/api/v3"},
		},
		{
			name: "empty api url is applied and left to validation",
			env:  map[string]string{EnvGitHubAPIURL: ""},
			want: GitHub{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := FromEnv(Default(), envFrom(tt.env))
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if got.GitHub != tt.want {
				t.Errorf("GitHub = %#v, want %#v", got.GitHub, tt.want)
			}
		})
	}
}

func TestGitHubValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		github  GitHub
		wantErr []string
		wantIs  error
	}{
		{name: "disabled", github: GitHub{APIURL: "https://api.github.com"}},
		{name: "key file", github: GitHub{AppID: "1", PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com"}},
		{name: "inline key", github: GitHub{AppID: "Iv23liAbCdEf", PrivateKey: fakeKey, APIURL: "https://api.github.com"}},
		{name: "enterprise url", github: GitHub{AppID: "1", PrivateKeyFile: "/key.pem", APIURL: "https://ghe.example.com/api/v3/"}},
		{name: "loopback http", github: GitHub{AppID: "1", PrivateKeyFile: "/key.pem", APIURL: "http://127.0.0.1:8081"}},
		{
			name:    "app id without key",
			github:  GitHub{AppID: "1", APIURL: "https://api.github.com"},
			wantErr: []string{"github app private key: required", EnvGitHubAppPrivateKeyFile, EnvGitHubAppPrivateKey},
		},
		{
			name:    "key without app id",
			github:  GitHub{PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com"},
			wantErr: []string{"github app id: required", EnvGitHubAppID},
		},
		{
			name:    "both key sources",
			github:  GitHub{AppID: "1", PrivateKey: fakeKey, PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com"},
			wantErr: []string{"set only one of"},
		},
		{
			name:    "app id with whitespace",
			github:  GitHub{AppID: "12 34", PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com"},
			wantErr: []string{"github app id: must not contain whitespace", `"12 34"`},
		},
		{
			name:    "plaintext remote url while disabled",
			github:  GitHub{APIURL: "http://ghe.example.com"},
			wantErr: []string{"github api url", "must use https"},
			wantIs:  github.ErrInvalidBaseURL,
		},
		{
			name:    "url with query",
			github:  GitHub{AppID: "1", PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com?x=1"},
			wantErr: []string{"github api url", "query"},
			wantIs:  github.ErrInvalidBaseURL,
		},
		{
			name:    "empty url",
			github:  GitHub{},
			wantErr: []string{"github api url"},
			wantIs:  github.ErrInvalidBaseURL,
		},
		{
			name:    "every error is reported",
			github:  GitHub{AppID: "a b", PrivateKey: fakeKey, PrivateKeyFile: "/key.pem", APIURL: "ftp://x"},
			wantErr: []string{"github api url", "github app id", "github app private key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Database.URL = "postgres://doorstep@localhost/doorstep"
			cfg.GitHub = tt.github
			err := cfg.Validate()
			assertErrorContains(t, err, tt.wantErr)
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("Validate() error %v does not wrap %v", err, tt.wantIs)
			}
			if err != nil && strings.Contains(err.Error(), "supersecret") {
				t.Errorf("error %q leaks the private key", err)
			}
		})
	}
}

func TestGitHubEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		github GitHub
		want   bool
	}{
		{name: "nothing set", github: GitHub{APIURL: "https://ghe.example.com"}, want: false},
		{name: "app id", github: GitHub{AppID: "1"}, want: true},
		{name: "inline key", github: GitHub{PrivateKey: "k"}, want: true},
		{name: "key file", github: GitHub{PrivateKeyFile: "/k"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.github.Enabled(); got != tt.want {
				t.Errorf("Enabled() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestSecretRedaction(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.GitHub = GitHub{AppID: "1", PrivateKey: fakeKey}

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config",
		slog.Any("key", cfg.GitHub.PrivateKey),
		slog.Any("github", cfg.GitHub),
	)

	tests := []struct {
		name string
		out  string
	}{
		{name: "String", out: cfg.GitHub.PrivateKey.String()},
		{name: "%v", out: fmt.Sprintf("%v", cfg)},
		{name: "%+v", out: fmt.Sprintf("%+v", cfg)},
		{name: "%#v", out: fmt.Sprintf("%#v", cfg)},
		{name: "%q", out: fmt.Sprintf("%q", cfg.GitHub.PrivateKey)},
		{name: "slog", out: logs.String()},
		{name: "json", out: mustJSON(t, cfg)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(tt.out, "supersecret") {
				t.Errorf("output leaks the secret: %s", tt.out)
			}
			if !strings.Contains(tt.out, "[redacted]") {
				t.Errorf("output does not show [redacted]: %s", tt.out)
			}
		})
	}
	if string(cfg.GitHub.PrivateKey) != fakeKey {
		t.Error("Secret does not keep its value")
	}
}

func TestEmptySecretIsShownEmpty(t *testing.T) {
	t.Parallel()

	var s Secret
	if got := fmt.Sprintf("%s|%#v|%v", s, s, s.LogValue()); got != "||" {
		t.Errorf("empty secret formats as %q, want empty", got)
	}
	if got := mustJSON(t, s); got != `""` {
		t.Errorf("empty secret JSON = %s", got)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}
