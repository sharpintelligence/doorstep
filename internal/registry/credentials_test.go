package registry

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func dockerConfigJSON(host, user, password string) string {
	return fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, b64(user+":"+password))
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	rewriteFile(t, path, content)
	return path
}

func rewriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestParseDockerConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    string
		want    map[string]Credential
		wantErr []string
	}{
		{
			name: "auth field",
			data: `{"auths":{"ghcr.io":{"auth":"` + b64("robot:s3cret") + `"}}}`,
			want: map[string]Credential{"ghcr.io": {Username: "robot", Password: "s3cret"}},
		},
		{
			name: "auth without padding",
			data: `{"auths":{"ghcr.io":{"auth":"` + strings.TrimRight(b64("ab:cd"), "=") + `"}}}`,
			want: map[string]Credential{"ghcr.io": {Username: "ab", Password: "cd"}},
		},
		{
			name: "username and password",
			data: `{"auths":{"registry.example.com":{"username":"robot","password":"s3cret"}}}`,
			want: map[string]Credential{"registry.example.com": {Username: "robot", Password: "s3cret"}},
		},
		{
			name: "auth wins over username and password",
			data: `{"auths":{"ghcr.io":{"auth":"` + b64("a:b") + `","username":"x","password":"y"}}}`,
			want: map[string]Credential{"ghcr.io": {Username: "a", Password: "b"}},
		},
		{
			name: "identity token",
			data: `{"auths":{"myregistry.azurecr.io":{"username":"00000000-0000-0000-0000-000000000000","identitytoken":"refresh"}}}`,
			want: map[string]Credential{"myregistry.azurecr.io": {Username: "00000000-0000-0000-0000-000000000000", IdentityToken: "refresh"}},
		},
		{
			name: "registry token",
			data: `{"auths":{"quay.io":{"registrytoken":"bearer-token"}}}`,
			want: map[string]Credential{"quay.io": {RegistryToken: "bearer-token"}},
		},
		{
			name: "password containing colons",
			data: `{"auths":{"ghcr.io":{"auth":"` + b64("robot:pa:ss:word") + `"}}}`,
			want: map[string]Credential{"ghcr.io": {Username: "robot", Password: "pa:ss:word"}},
		},
		{
			name: "legacy docker hub key",
			data: `{"auths":{"https://index.docker.io/v1/":{"auth":"` + b64("hub:pw") + `"}}}`,
			want: map[string]Credential{"index.docker.io": {Username: "hub", Password: "pw"}},
		},
		{
			name: "keys are normalized",
			data: `{"auths":{
				"https://GHCR.io/Owner-A/":{"auth":"` + b64("a:1") + `"},
				"http://harbor.local:8443/v2/":{"auth":"` + b64("h:2") + `"},
				"registry-1.docker.io":{"auth":"` + b64("d:3") + `"}
			}}`,
			want: map[string]Credential{
				"ghcr.io/owner-a":   {Username: "a", Password: "1"},
				"harbor.local:8443": {Username: "h", Password: "2"},
				"index.docker.io":   {Username: "d", Password: "3"},
			},
		},
		{
			name: "unknown fields and credential helpers are ignored",
			data: `{"auths":{"ghcr.io":{"auth":"` + b64("a:b") + `","email":"me@example.com"}},"credsStore":"desktop","credHelpers":{"gcr.io":"gcloud"},"HttpHeaders":{"X":"y"}}`,
			want: map[string]Credential{"ghcr.io": {Username: "a", Password: "b"}},
		},
		{
			name: "missing auths is an empty set",
			data: `{"credsStore":"desktop"}`,
			want: map[string]Credential{},
		},
		{
			name: "null document is an empty set",
			data: `null`,
			want: map[string]Credential{},
		},
		{
			name:    "bad base64",
			data:    `{"auths":{"ghcr.io":{"auth":"!!!not-base64!!!"}}}`,
			wantErr: []string{`auths["ghcr.io"]`, "not valid base64"},
		},
		{
			name:    "auth without colon",
			data:    `{"auths":{"ghcr.io":{"auth":"` + b64("justauser") + `"}}}`,
			wantErr: []string{`auths["ghcr.io"]`, "username:password"},
		},
		{
			name:    "auth with empty password",
			data:    `{"auths":{"ghcr.io":{"auth":"` + b64("robot:") + `"}}}`,
			wantErr: []string{`auths["ghcr.io"]`, "empty password"},
		},
		{
			name:    "auth with empty username",
			data:    `{"auths":{"ghcr.io":{"auth":"` + b64(":s3cret") + `"}}}`,
			wantErr: []string{`auths["ghcr.io"]`, "empty username"},
		},
		{
			name: "path ending in v2 stays scoped",
			data: `{"auths":{"ghcr.io/acme/v2":{"auth":"` + b64("a:b") + `"}}}`,
			want: map[string]Credential{"ghcr.io/acme/v2": {Username: "a", Password: "b"}},
		},
		{
			name:    "empty entry",
			data:    `{"auths":{"ghcr.io":{}}}`,
			wantErr: []string{`auths["ghcr.io"]`, "no usable credential"},
		},
		{
			name:    "username without password",
			data:    `{"auths":{"ghcr.io":{"username":"robot"}}}`,
			wantErr: []string{`auths["ghcr.io"]`, "no usable credential"},
		},
		{
			name:    "empty key",
			data:    `{"auths":{"https://":{"auth":"` + b64("a:b") + `"}}}`,
			wantErr: []string{"registry host must not be empty"},
		},
		{
			name:    "glob key",
			data:    `{"auths":{"*.example.com":{"auth":"` + b64("a:b") + `"}}}`,
			wantErr: []string{`auths["*.example.com"]`, "registry host"},
		},
		{
			name:    "aliases collide",
			data:    `{"auths":{"docker.io":{"auth":"` + b64("a:b") + `"},"https://index.docker.io/v1/":{"auth":"` + b64("c:d") + `"}}}`,
			wantErr: []string{"same registry", "index.docker.io"},
		},
		{
			name:    "every bad entry is reported",
			data:    `{"auths":{"a.io":{},"b.io":{"auth":"%%%"}}}`,
			wantErr: []string{`auths["a.io"]`, `auths["b.io"]`},
		},
		{
			name:    "not JSON",
			data:    `auths: {}`,
			wantErr: []string{"not valid JSON"},
		},
		{
			name:    "wrong shape",
			data:    `{"auths":["ghcr.io"]}`,
			wantErr: []string{"not a Docker config"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDockerConfig([]byte(tt.data))
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ParseDockerConfig() = %v, want error", got)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				for _, secret := range []string{"s3cret", "!!!not-base64!!!", "justauser"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error %q leaks %q", err, secret)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDockerConfig: %v", err)
			}
			if got.Len() != len(tt.want) {
				t.Errorf("Len() = %d, want %d (keys %v)", got.Len(), len(tt.want), got.Keys())
			}
			for key, want := range tt.want {
				host, repo, _ := strings.Cut(key, "/")
				if repo == "" {
					repo = "some/repo"
				}
				cred, ok, err := got.Lookup(host, repo)
				if err != nil || !ok {
					t.Errorf("Lookup(%q, %q) = %v, %v, %v", host, repo, cred, ok, err)
					continue
				}
				if cred != want {
					t.Errorf("Lookup(%q, %q) = %v, want %v", host, repo, cred, want)
				}
			}
		})
	}
}

func TestCredentialsLookup(t *testing.T) {
	t.Parallel()

	creds, err := ParseDockerConfig([]byte(`{"auths":{
		"ghcr.io":{"username":"ghcr","password":"p"},
		"ghcr.io/owner-a":{"username":"owner-a","password":"p"},
		"ghcr.io/owner-a/special":{"username":"special","password":"p"},
		"ghcr.io/own":{"username":"own","password":"p"},
		"ghcr.io/acme/v2":{"username":"acme-v2","password":"p"},
		"docker.io":{"username":"hub","password":"p"},
		"localhost:5000":{"username":"local","password":"p"}
	}}`))
	if err != nil {
		t.Fatalf("ParseDockerConfig: %v", err)
	}

	tests := []struct {
		name       string
		host       string
		repository string
		wantUser   string
	}{
		{name: "host match", host: "ghcr.io", repository: "someone/app", wantUser: "ghcr"},
		{name: "host is case insensitive", host: "GHCR.IO", repository: "someone/app", wantUser: "ghcr"},
		{name: "owner prefix beats host", host: "ghcr.io", repository: "owner-a/app", wantUser: "owner-a"},
		{name: "longest prefix wins", host: "ghcr.io", repository: "owner-a/special/app", wantUser: "special"},
		{name: "exact repository prefix", host: "ghcr.io", repository: "owner-a/special", wantUser: "special"},
		{name: "sibling of longer prefix", host: "ghcr.io", repository: "owner-a/specialist", wantUser: "owner-a"},
		{name: "segment boundary", host: "ghcr.io", repository: "owner/app", wantUser: "ghcr"},
		{name: "partial segment does not match", host: "ghcr.io", repository: "owner-b/app", wantUser: "ghcr"},
		{name: "prefix itself matches", host: "ghcr.io", repository: "own/app", wantUser: "own"},
		{name: "path ending in v2 is scoped to that path", host: "ghcr.io", repository: "acme/v2/app", wantUser: "acme-v2"},
		{name: "path ending in v2 does not cover siblings", host: "ghcr.io", repository: "acme/other", wantUser: "ghcr"},
		{name: "index.docker.io", host: "index.docker.io", repository: "library/nginx", wantUser: "hub"},
		{name: "docker.io alias", host: "docker.io", repository: "owner/app", wantUser: "hub"},
		{name: "registry-1 alias", host: "registry-1.docker.io", repository: "owner/app", wantUser: "hub"},
		{name: "host with port", host: "localhost:5000", repository: "app", wantUser: "local"},
		{name: "different port", host: "localhost:5001", repository: "app"},
		{name: "no match", host: "quay.io", repository: "owner/app"},
		{name: "host suffix does not match", host: "evilghcr.io", repository: "owner-a/app"},
		{name: "host prefix does not match", host: "ghcr.io.evil.com", repository: "owner-a/app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := creds.Lookup(tt.host, tt.repository)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if ok != (tt.wantUser != "") {
				t.Fatalf("Lookup(%q, %q) found = %v, want %v", tt.host, tt.repository, ok, tt.wantUser != "")
			}
			if got.Username != tt.wantUser {
				t.Errorf("Lookup(%q, %q) user = %q, want %q", tt.host, tt.repository, got.Username, tt.wantUser)
			}
		})
	}
}

func TestEmptyCredentialsLookup(t *testing.T) {
	t.Parallel()

	_, ok, err := Credentials{}.Lookup("ghcr.io", "owner/app")
	if ok || err != nil {
		t.Errorf("Lookup on empty set = %v, %v, want no match", ok, err)
	}
}

func TestFileCredentials(t *testing.T) {
	t.Parallel()

	path := writeFile(t, dockerConfigJSON("ghcr.io", "first", "one"))
	source := FileCredentials{Path: path}

	tests := []struct {
		name     string
		content  string
		wantUser string
		wantOK   bool
	}{
		{name: "initial file", wantUser: "first", wantOK: true},
		{name: "rotated secret", content: dockerConfigJSON("ghcr.io", "second", "two"), wantUser: "second", wantOK: true},
		{name: "entry removed", content: dockerConfigJSON("quay.io", "third", "three"), wantOK: false},
	}

	for _, tt := range tests {
		if tt.content != "" {
			rewriteFile(t, path, tt.content)
		}
		got, ok, err := source.Lookup("ghcr.io", "owner/app")
		if err != nil {
			t.Fatalf("%s: Lookup: %v", tt.name, err)
		}
		if ok != tt.wantOK || got.Username != tt.wantUser {
			t.Errorf("%s: Lookup = %q, %v, want %q, %v", tt.name, got.Username, ok, tt.wantUser, tt.wantOK)
		}
	}
}

func TestLoadDockerConfigFileErrors(t *testing.T) {
	t.Parallel()

	secret := "hunter2-super-secret"
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantIs  error
		wantErr []string
	}{
		{
			name:    "missing file",
			path:    func(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "absent.json") },
			wantIs:  fs.ErrNotExist,
			wantErr: []string{"read registry credentials file", "absent.json"},
		},
		{
			name: "malformed file",
			path: func(t *testing.T) string {
				t.Helper()
				return writeFile(t, `{"auths":{"ghcr.io":{"password":"`+secret+`"`)
			},
			wantErr: []string{"parse registry credentials file", "config.json", "not valid JSON"},
		},
		{
			name: "invalid entry",
			path: func(t *testing.T) string {
				t.Helper()
				return writeFile(t, `{"auths":{"ghcr.io":{"password":"`+secret+`"}}}`)
			},
			wantErr: []string{"parse registry credentials file", "no usable credential"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := tt.path(t)
			for _, load := range []func() error{
				func() error { _, err := LoadDockerConfigFile(path); return err },
				func() error { _, _, err := FileCredentials{Path: path}.Lookup("ghcr.io", "a/b"); return err },
			} {
				err := load()
				if err == nil {
					t.Fatal("expected error")
				}
				if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
					t.Errorf("error %v is not %v", err, tt.wantIs)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q leaks file contents", err)
				}
			}
		})
	}
}

func TestCredentialRedaction(t *testing.T) {
	t.Parallel()

	cred := Credential{Username: "robot", Password: "pw-secret", IdentityToken: "id-secret", RegistryToken: "reg-secret"}
	creds := mustCredentials(t, map[string]Credential{"ghcr.io": cred})

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("credential", slog.Any("credential", cred), slog.Any("credentials", creds))
	textLogger := slog.New(slog.NewTextHandler(&logs, nil))
	textLogger.Info("credential", "credential", cred, "credentials", creds)

	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v", cred),
		"%+v":  fmt.Sprintf("%+v", cred),
		"%#v":  fmt.Sprintf("%#v", cred),
		"%s":   fmt.Sprintf("<%s>", cred),
		"set":  fmt.Sprintf("%v %+v %#v", creds, creds, creds),
		"ptr":  fmt.Sprintf("%v", &cred),
		"logs": logs.String(),
	}
	for format, out := range outputs {
		for _, secret := range []string{"pw-secret", "id-secret", "reg-secret"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s output %q leaks %q", format, out, secret)
			}
		}
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "logs"} {
		if !strings.Contains(outputs[format], "robot") {
			t.Errorf("%s output %q does not show the username", format, outputs[format])
		}
		if !strings.Contains(outputs[format], redacted) {
			t.Errorf("%s output %q does not mark secrets as redacted", format, outputs[format])
		}
	}
	if !strings.Contains(outputs["set"], "ghcr.io") {
		t.Errorf("credentials set output %q does not list keys", outputs["set"])
	}
	if got := creds.Keys(); !slices.Equal(got, []string{"ghcr.io"}) {
		t.Errorf("Keys() = %v", got)
	}
}

func TestNormalizeKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{raw: "ghcr.io", want: "ghcr.io"},
		{raw: "GHCR.IO", want: "ghcr.io"},
		{raw: " ghcr.io ", want: "ghcr.io"},
		{raw: "https://ghcr.io/", want: "ghcr.io"},
		{raw: "http://registry.local:5000", want: "registry.local:5000"},
		{raw: "https://index.docker.io/v1/", want: "index.docker.io"},
		{raw: "https://registry.example.com/v2/", want: "registry.example.com"},
		{raw: "docker.io", want: "index.docker.io"},
		{raw: "registry-1.docker.io", want: "index.docker.io"},
		{raw: "docker.io/owner", want: "index.docker.io/owner"},
		{raw: "ghcr.io/owner-a", want: "ghcr.io/owner-a"},
		{raw: "ghcr.io/owner-a/", want: "ghcr.io/owner-a"},
		{raw: "ghcr.io/v1", want: "ghcr.io"},
		{raw: "https://ghcr.io/v2/", want: "ghcr.io"},
		{raw: "ghcr.io/owner-a/v2", want: "ghcr.io/owner-a/v2"},
		{raw: "ghcr.io/owner-a/v1/", want: "ghcr.io/owner-a/v1"},
		{raw: "ghcr.io/v2/app", want: "ghcr.io/v2/app"},
		{raw: "", wantErr: true},
		{raw: "https://", wantErr: true},
		{raw: "/owner", wantErr: true},
		{raw: "*.example.com", wantErr: true},
		{raw: "ghcr.io//owner", wantErr: true},
		{raw: "ghcr.io/own er", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeKey(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("normalizeKey(%q) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeKey(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("normalizeKey(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
