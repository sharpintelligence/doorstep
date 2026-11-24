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
			name: "webhook secret file",
			env:  map[string]string{EnvGitHubWebhookSecretFile: "/etc/doorstep/secrets/github-webhook-secret"},
			want: GitHub{WebhookSecretFile: "/etc/doorstep/secrets/github-webhook-secret", APIURL: "https://api.github.com"},
		},
		{
			name: "inline webhook secret",
			env:  map[string]string{EnvGitHubWebhookSecret: fakeKey},
			want: GitHub{WebhookSecret: fakeKey, APIURL: "https://api.github.com"},
		},
		{
			name: "both webhook secret sources are kept for validation",
			env:  map[string]string{EnvGitHubWebhookSecret: fakeKey, EnvGitHubWebhookSecretFile: "/secret"},
			want: GitHub{WebhookSecret: fakeKey, WebhookSecretFile: "/secret", APIURL: "https://api.github.com"},
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

func TestGitHubFromEnvWebhookSecretReplacesFileSetting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
		want GitHub
	}{
		{
			name: "inline environment value replaces a file from the config file",
			env:  map[string]string{EnvGitHubWebhookSecret: fakeKey},
			want: GitHub{WebhookSecret: fakeKey, APIURL: "https://api.github.com"},
		},
		{
			name: "file from the environment replaces a file from the config file",
			env:  map[string]string{EnvGitHubWebhookSecretFile: "/env/secret"},
			want: GitHub{WebhookSecretFile: "/env/secret", APIURL: "https://api.github.com"},
		},
		{
			name: "unset keeps the config file setting",
			env:  map[string]string{},
			want: GitHub{WebhookSecretFile: "/file/secret", APIURL: "https://api.github.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := Default()
			base.GitHub.WebhookSecretFile = "/file/secret"
			got, err := FromEnv(base, envFrom(tt.env))
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
		{name: "webhook secret file", github: GitHub{AppID: "1", PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com", WebhookSecretFile: "/webhook"}},
		{name: "inline webhook secret", github: GitHub{AppID: "1", PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com", WebhookSecret: fakeKey}},
		{
			name:    "both webhook secret sources",
			github:  GitHub{AppID: "1", PrivateKeyFile: "/key.pem", APIURL: "https://api.github.com", WebhookSecret: fakeKey, WebhookSecretFile: "/webhook"},
			wantErr: []string{"github webhook secret: set only one of", EnvGitHubWebhookSecretFile, EnvGitHubWebhookSecret},
		},
		{
			name:    "webhook secret without the app",
			github:  GitHub{APIURL: "https://api.github.com", WebhookSecretFile: "/webhook"},
			wantErr: []string{"github webhook secret: requires the GitHub App", EnvGitHubAppID},
		},
		{
			name:    "inline webhook secret without the app",
			github:  GitHub{APIURL: "https://api.github.com", WebhookSecret: fakeKey},
			wantErr: []string{"github webhook secret: requires the GitHub App"},
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
		{name: "webhook secret alone", github: GitHub{WebhookSecretFile: "/w"}, want: false},
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

func TestGitHubWebhooksEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		github GitHub
		want   bool
	}{
		{name: "nothing set", github: GitHub{AppID: "1", PrivateKeyFile: "/k"}, want: false},
		{name: "inline secret", github: GitHub{WebhookSecret: "s"}, want: true},
		{name: "secret file", github: GitHub{WebhookSecretFile: "/s"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.github.WebhooksEnabled(); got != tt.want {
				t.Errorf("WebhooksEnabled() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestGitHubReadWebhookSecret(t *testing.T) {
	t.Parallel()

	errMissing := errors.New("no such file")
	files := map[string]string{
		"/secret":       "file-secret\n",
		"/crlf":         "crlf-secret\r\n",
		"/two-newlines": "kept\n\n",
		"/empty":        "",
		"/newline":      "\n",
	}
	readFile := func(name string) ([]byte, error) {
		if v, ok := files[name]; ok {
			return []byte(v), nil
		}
		return nil, errMissing
	}
	tests := []struct {
		name    string
		github  GitHub
		want    string
		wantErr []string
		wantIs  error
	}{
		{name: "disabled", github: GitHub{}},
		{name: "inline", github: GitHub{WebhookSecret: fakeKey}, want: fakeKey},
		{name: "file trims one trailing newline", github: GitHub{WebhookSecretFile: "/secret"}, want: "file-secret"},
		{name: "file trims a trailing CRLF", github: GitHub{WebhookSecretFile: "/crlf"}, want: "crlf-secret"},
		{name: "file keeps all but one newline", github: GitHub{WebhookSecretFile: "/two-newlines"}, want: "kept\n"},
		{name: "empty file", github: GitHub{WebhookSecretFile: "/empty"}, wantErr: []string{"github webhook secret", "file /empty is empty"}},
		{name: "newline only", github: GitHub{WebhookSecretFile: "/newline"}, wantErr: []string{"is empty"}},
		{name: "missing file", github: GitHub{WebhookSecretFile: "/missing"}, wantErr: []string{"read github webhook secret"}, wantIs: errMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.github.ReadWebhookSecret(readFile)
			assertErrorContains(t, err, tt.wantErr)
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tt.wantIs)
			}
			if string(got) != tt.want {
				t.Errorf("secret = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSecretRedaction(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.GitHub = GitHub{AppID: "1", PrivateKey: fakeKey, WebhookSecret: fakeKey}

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
