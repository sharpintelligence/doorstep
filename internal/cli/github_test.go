package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/github"
)

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestNewGitHubApp(t *testing.T) {
	t.Parallel()

	keyPEM := testKeyPEM(t)
	corruptPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("supersecret")})
	const keyPath = "/var/run/secrets/github/private-key.pem"
	files := map[string][]byte{
		keyPath:        keyPEM,
		"/garbage.pem": []byte("not a key"),
	}
	readFile := func(name string) ([]byte, error) {
		if b, ok := files[name]; ok {
			return b, nil
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	apiURL := github.DefaultBaseURL

	tests := []struct {
		name    string
		cfg     config.GitHub
		wantApp bool
		wantErr string
		wantIs  error
	}{
		{name: "disabled", cfg: config.GitHub{APIURL: apiURL}},
		{name: "key from file", cfg: config.GitHub{AppID: "1", PrivateKeyFile: keyPath, APIURL: apiURL}, wantApp: true},
		{name: "inline key", cfg: config.GitHub{AppID: "1", PrivateKey: config.Secret(keyPEM), APIURL: apiURL}, wantApp: true},
		{
			name:    "inline key with escaped newlines",
			cfg:     config.GitHub{AppID: "1", PrivateKey: config.Secret(strings.ReplaceAll(string(keyPEM), "\n", `\n`)), APIURL: apiURL},
			wantApp: true,
		},
		{
			name:    "enterprise url",
			cfg:     config.GitHub{AppID: "1", PrivateKeyFile: keyPath, APIURL: "https://ghe.example.com/api/v3"},
			wantApp: true,
		},
		{
			name:    "unreadable file",
			cfg:     config.GitHub{AppID: "1", PrivateKeyFile: "/missing.pem", APIURL: apiURL},
			wantErr: "read github app private key",
			wantIs:  fs.ErrNotExist,
		},
		{
			name:    "bad key file",
			cfg:     config.GitHub{AppID: "1", PrivateKeyFile: "/garbage.pem", APIURL: apiURL},
			wantErr: "github app private key",
			wantIs:  github.ErrInvalidPrivateKey,
		},
		{
			name:    "bad inline key",
			cfg:     config.GitHub{AppID: "1", PrivateKey: config.Secret(corruptPEM), APIURL: apiURL},
			wantErr: "github app private key",
			wantIs:  github.ErrInvalidPrivateKey,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app, err := newGitHubApp(tt.cfg, readFile, slog.New(slog.DiscardHandler))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				if !errors.Is(err, tt.wantIs) {
					t.Errorf("error %v does not wrap %v", err, tt.wantIs)
				}
				if strings.Contains(err.Error(), "c3VwZXJzZWNyZXQ") {
					t.Errorf("error %q leaks key material", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("newGitHubApp: %v", err)
			}
			if (app != nil) != tt.wantApp {
				t.Errorf("app = %v, want app %t", app, tt.wantApp)
			}
		})
	}
}

func TestLogGitHubApp(t *testing.T) {
	t.Parallel()

	cfg := config.GitHub{AppID: "Iv23liAbCdEf", PrivateKey: config.Secret(testKeyPEM(t)), APIURL: "https://ghe.example.com/api/v3"}
	app, err := newGitHubApp(cfg, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newGitHubApp: %v", err)
	}

	tests := []struct {
		name string
		app  *github.App
		want []string
	}{
		{name: "configured", app: app, want: []string{`"msg":"github app configured"`, `"app_id":"Iv23liAbCdEf"`, `"api_url":"https://ghe.example.com/api/v3"`}},
		{name: "not configured", want: []string{`"msg":"github app not configured"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logGitHubApp(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), cfg, tt.app)
			for _, want := range tt.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("log %q does not contain %q", buf.String(), want)
				}
			}
			if strings.Contains(buf.String(), "PRIVATE KEY") {
				t.Errorf("log leaks the private key: %s", buf.String())
			}
		})
	}
}
