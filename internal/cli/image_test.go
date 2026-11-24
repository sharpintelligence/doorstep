package cli

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/config"
)

const imageDigest = "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"

func newImageRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "robot" || password != "s3cret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/team/app/manifests/v1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			w.Header().Set("Content-Length", "2")
			w.Header().Set("Docker-Content-Digest", imageDigest)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func writeDockerConfig(t *testing.T, host, user, password string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	content := fmt.Sprintf(`{"auths":{%q:{"username":%q,"password":%q}}}`, host, user, password)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestRunImageResolve(t *testing.T) {
	t.Parallel()

	host := newImageRegistry(t)
	good := writeDockerConfig(t, host, "robot", "s3cret")
	bad := writeDockerConfig(t, host, "robot", "wrong")

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string
		wantStderr []string
	}{
		{
			name:       "credentials from flag",
			args:       []string{"image", "resolve", host + "/team/app", "v1", "--registry-credentials-file", good},
			wantStdout: imageDigest + "\n",
		},
		{
			name:       "credentials from environment",
			args:       []string{"image", "resolve", host + "/team/app", "v1"},
			env:        map[string]string{config.EnvRegistryCredentialsFile: good},
			wantStdout: imageDigest + "\n",
		},
		{
			name:       "flag beats environment",
			args:       []string{"image", "resolve", host + "/team/app", "v1", "--registry-credentials-file", good},
			env:        map[string]string{config.EnvRegistryCredentialsFile: bad},
			wantStdout: imageDigest + "\n",
		},
		{
			name:       "not found",
			args:       []string{"image", "resolve", host + "/team/app", "v2", "--registry-credentials-file", good},
			wantCode:   1,
			wantStderr: []string{"doorstep: resolve " + host + "/team/app:v2", "image not found"},
		},
		{
			name:       "anonymous is denied",
			args:       []string{"image", "resolve", host + "/team/app", "v1"},
			wantCode:   1,
			wantStderr: []string{"registry denied access"},
		},
		{
			name:       "wrong credentials are denied",
			args:       []string{"image", "resolve", host + "/team/app", "v1"},
			env:        map[string]string{config.EnvRegistryCredentialsFile: bad},
			wantCode:   1,
			wantStderr: []string{"registry denied access"},
		},
		{
			name:       "invalid reference",
			args:       []string{"image", "resolve", host + "/team/app:v1", "v1"},
			wantCode:   1,
			wantStderr: []string{"invalid image reference"},
		},
		{
			name:       "missing credentials file",
			args:       []string{"image", "resolve", host + "/team/app", "v1", "--registry-credentials-file", filepath.Join(t.TempDir(), "absent.json")},
			wantCode:   1,
			wantStderr: []string{"doorstep: registry credentials: read registry credentials file"},
		},
		{
			name:       "missing tag argument",
			args:       []string{"image", "resolve", host + "/team/app"},
			wantCode:   1,
			wantStderr: []string{"accepts 2 arg(s), received 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := Run(t.Context(), tt.args, &stdout, &stderr, envFrom(tt.env))
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr.String())
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
			if strings.Contains(stderr.String(), "s3cret") {
				t.Errorf("stderr leaks credentials: %q", stderr.String())
			}
		})
	}
}

func TestRunImageHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"image", "resolve", "--help"}, &stdout, &stderr, envFrom(nil)); code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	for _, want := range []string{"resolve <repository> <tag>", "--registry-credentials-file", config.EnvRegistryCredentialsFile, "index digest"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help %q does not mention %q", stdout.String(), want)
		}
	}
}
