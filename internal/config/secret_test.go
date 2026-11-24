package config

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

const secretValue = "hunter2-very-secret"

func filesFrom(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		v, ok := files[name]
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return []byte(v), nil
	}
}

func TestSecretRefRead(t *testing.T) {
	t.Parallel()

	env := envFrom(map[string]string{
		"PLAIN":    secretValue,
		"NEWLINE":  secretValue + "\n",
		"CRLF":     secretValue + "\r\n",
		"TWO":      secretValue + "\n\n",
		"EMPTY":    "",
		"ONLY_EOL": "\n",
	})
	files := filesFrom(map[string]string{
		"/run/secrets/plain":   secretValue,
		"/run/secrets/newline": secretValue + "\n",
		"/run/secrets/empty":   "",
	})

	tests := []struct {
		name    string
		ref     SecretRef
		want    string
		wantErr []string
	}{
		{name: "inline value", ref: SecretRef{Label: "x", Value: "https://example.com"}, want: "https://example.com"},
		{name: "env", ref: SecretRef{Label: "x", Env: "PLAIN"}, want: secretValue},
		{name: "env trailing newline", ref: SecretRef{Label: "x", Env: "NEWLINE"}, want: secretValue},
		{name: "env trailing crlf", ref: SecretRef{Label: "x", Env: "CRLF"}, want: secretValue},
		{name: "only one newline trimmed", ref: SecretRef{Label: "x", Env: "TWO"}, want: secretValue + "\n"},
		{name: "file", ref: SecretRef{Label: "x", File: "/run/secrets/plain"}, want: secretValue},
		{name: "file trailing newline", ref: SecretRef{Label: "x", File: "/run/secrets/newline"}, want: secretValue},
		{
			name:    "env unset",
			ref:     SecretRef{Label: "notification ops webhook_url", Env: "MISSING"},
			wantErr: []string{"notification ops webhook_url: environment variable MISSING is not set"},
		},
		{
			name:    "env empty",
			ref:     SecretRef{Label: "notification ops webhook_url", Env: "EMPTY"},
			wantErr: []string{"notification ops webhook_url: environment variable EMPTY is empty"},
		},
		{
			name:    "env only newline",
			ref:     SecretRef{Label: "x", Env: "ONLY_EOL"},
			wantErr: []string{"environment variable ONLY_EOL is empty"},
		},
		{
			name:    "file unreadable",
			ref:     SecretRef{Label: "notification hook secret", File: "/run/secrets/missing"},
			wantErr: []string{"notification hook secret: open /run/secrets/missing", "file does not exist"},
		},
		{
			name:    "file empty",
			ref:     SecretRef{Label: "x", File: "/run/secrets/empty"},
			wantErr: []string{"x: file /run/secrets/empty is empty"},
		},
		{
			name:    "no source",
			ref:     SecretRef{Label: "x"},
			wantErr: []string{"x: no value, environment variable or file configured"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.ref.Read(env, files)
			assertErrorContains(t, err, tt.wantErr)
			if got != tt.want {
				t.Errorf("Read() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSecretRefReadEnvWinsOverFile(t *testing.T) {
	t.Parallel()

	ref := SecretRef{Label: "x", Env: "A", File: "/a"}
	got, err := ref.Read(envFrom(map[string]string{"A": "from-env"}), filesFrom(map[string]string{"/a": "from-file"}))
	if err != nil || got != "from-env" {
		t.Errorf("Read() = %q, %v; want from-env", got, err)
	}
}

func TestCatalogCheckSecrets(t *testing.T) {
	t.Parallel()

	catalog := Catalog{Notifications: []Notification{
		{Name: "ops", Type: NotifySlack, WebhookURLEnv: "SLACK_URL"},
		{Name: "hook", Type: NotifyWebhook, URLFile: "/run/secrets/hook-url", SecretEnv: "HOOK_SECRET"},
		{Name: "public", Type: NotifyWebhook, URL: "https://hooks.example.com"},
	}}

	tests := []struct {
		name    string
		env     map[string]string
		files   map[string]string
		wantErr []string
	}{
		{
			name:  "all resolved",
			env:   map[string]string{"SLACK_URL": secretValue, "HOOK_SECRET": secretValue},
			files: map[string]string{"/run/secrets/hook-url": "https://hooks.example.com/" + secretValue},
		},
		{
			name: "every failure reported",
			env:  map[string]string{"HOOK_SECRET": ""},
			wantErr: []string{
				"notification ops webhook_url: environment variable SLACK_URL is not set",
				"notification hook url: open /run/secrets/hook-url",
				"notification hook secret: environment variable HOOK_SECRET is empty",
			},
		},
		{
			name:    "one missing",
			env:     map[string]string{"SLACK_URL": secretValue, "HOOK_SECRET": secretValue + "\n"},
			wantErr: []string{"notification hook url"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := catalog.CheckSecrets(envFrom(tt.env), filesFrom(tt.files))
			assertErrorContains(t, err, tt.wantErr)
			if err != nil && strings.Contains(err.Error(), secretValue) {
				t.Errorf("error leaks the secret value: %v", err)
			}
		})
	}
}

func TestSecretRefReadNeverLeaksValue(t *testing.T) {
	t.Parallel()

	readErr := errors.New("permission denied")
	refs := []SecretRef{
		{Label: "x", Env: "EMPTY"},
		{Label: "x", File: "/denied"},
	}
	for _, ref := range refs {
		_, err := ref.Read(
			envFrom(map[string]string{"EMPTY": "\n", "OTHER": secretValue}),
			func(string) ([]byte, error) { return []byte(secretValue), readErr },
		)
		if err == nil {
			t.Fatalf("Read(%+v) = nil error", ref)
		}
		if strings.Contains(err.Error(), secretValue) {
			t.Errorf("Read(%+v) error leaks the secret: %v", ref, err)
		}
	}
}
