package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBlockCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		status     int
		body       string
		wantMethod string
		wantPath   string
		wantBody   string
		wantStdout string
	}{
		{
			name:       "block a digest",
			args:       []string{"block", "blog", cliBadDigest, "--reason", "checkout errors"},
			status:     http.StatusCreated,
			body:       blockJSON,
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/apps/blog/blocks",
			wantBody:   `{"digest":"` + cliBadDigest + `","reason":"checkout errors"}`,
			wantStdout: "blocked " + cliBadDigest + " (commit " + cliBadSHA + ") for blog by user:alice: checkout errors\n",
		},
		{
			name:       "block a commit",
			args:       []string{"block", "blog", cliBadSHA, "--reason", "bad migration"},
			status:     http.StatusOK,
			body:       `{"id":4,"commit":{"sha":"` + cliBadSHA + `"},"reason":"bad migration","created_by":"user:alice"}`,
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/apps/blog/blocks",
			wantBody:   `{"commit_sha":"` + cliBadSHA + `","reason":"bad migration"}`,
			wantStdout: "blocked commit " + cliBadSHA + " for blog by user:alice: bad migration\n",
		},
		{
			name:       "unblock a digest",
			args:       []string{"unblock", "blog", cliBadDigest},
			status:     http.StatusOK,
			body:       `{"blocks":[` + blockJSON + `]}`,
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v1/apps/blog/blocks/" + cliBadDigest,
			wantStdout: "unblocked " + cliBadDigest + " (commit " + cliBadSHA + ") for blog\n",
		},
		{
			name:       "unblock a commit with two blocks",
			args:       []string{"unblock", "blog", cliBadSHA},
			status:     http.StatusOK,
			body:       `{"blocks":[{"id":1,"commit":{"sha":"` + cliBadSHA + `"}},{"id":2,"digest":"` + cliGoodDigest + `","commit":{"sha":"` + cliBadSHA + `"}}]}`,
			wantMethod: http.MethodDelete,
			wantPath:   "/api/v1/apps/blog/blocks/" + cliBadSHA,
			wantStdout: "unblocked commit " + cliBadSHA + " for blog\nunblocked " + cliGoodDigest + " (commit " + cliBadSHA + ") for blog\n",
		},
		{
			name:       "list blocks",
			args:       []string{"blocks", "blog"},
			status:     http.StatusOK,
			body:       `{"blocks":[` + blockJSON + `,{"id":4,"commit":{"sha":"` + cliBadSHA + `"},"reason":"r","created_by":"user:bob","created_at":"2026-11-23T13:00:00Z","recorded_in_git":true}]}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/apps/blog/blocks",
			wantStdout: "ID  DIGEST               COMMIT        BY          SINCE                 IN GIT  REASON\n" +
				"3   sha256:cccccccccccc  cccccccccccc  user:alice  2026-11-23T12:00:00Z  no      checkout errors\n" +
				"4   -                    cccccccccccc  user:bob    2026-11-23T13:00:00Z  yes     r\n",
		},
		{
			name:       "list no blocks",
			args:       []string{"blocks", "blog"},
			status:     http.StatusOK,
			body:       `{"blocks":[]}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/apps/blog/blocks",
			wantStdout: "no blocks for blog\n",
		},
		{
			name:       "list blocks as JSON",
			args:       []string{"blocks", "blog", "-o", "json"},
			status:     http.StatusOK,
			body:       `{"blocks":[]}`,
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/apps/blog/blocks",
			wantStdout: "{\n  \"blocks\": []\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: tt.status, body: tt.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			code, stdout, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"})
			if code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr)
			}
			if stdout != tt.wantStdout {
				t.Errorf("stdout =\n%s\nwant\n%s", stdout, tt.wantStdout)
			}
			req := fake.only(t)
			if req.method != tt.wantMethod || req.path != tt.wantPath || req.auth != "Bearer tok-123" {
				t.Errorf("request = %s %s (auth %q), want %s %s", req.method, req.path, req.auth, tt.wantMethod, tt.wantPath)
			}
			if req.body != tt.wantBody {
				t.Errorf("body = %q, want %q", req.body, tt.wantBody)
			}
		})
	}
}

func TestBlockCommandErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		args         []string
		status       int
		body         string
		wantStderr   string
		wantRequests int
	}{
		{name: "block without reason", args: []string{"block", "blog", cliBadSHA}, wantStderr: `required flag(s) "reason" not set`},
		{name: "block with bad ref", args: []string{"block", "blog", "main", "--reason", "r"}, wantStderr: "must be a sha256 digest or a 40 character"},
		{name: "block with bad digest", args: []string{"block", "blog", "sha256:abc", "--reason", "r"}, wantStderr: "invalid image digest"},
		{name: "block with bad app", args: []string{"block", "..", cliBadSHA, "--reason", "r"}, wantStderr: "app name"},
		{name: "unblock without ref", args: []string{"unblock", "blog"}, wantStderr: "accepts 2 arg"},
		{name: "unblock with uppercase sha", args: []string{"unblock", "blog", strings.ToUpper(cliBadSHA)}, wantStderr: "must be a sha256 digest"},
		{name: "blocks with bad output", args: []string{"blocks", "blog", "-o", "yaml"}, wantStderr: `--output: must be table or json`},
		{name: "blocks with bad app", args: []string{"blocks", "a/b"}, wantStderr: "app name"},
		{
			name:         "unblock what is not blocked",
			args:         []string{"unblock", "blog", cliBadSHA},
			status:       http.StatusNotFound,
			body:         `{"error":{"code":"not_found","message":"` + cliBadSHA + ` is not blocked for blog"}}`,
			wantStderr:   "doorstep: " + cliBadSHA + " is not blocked for blog",
			wantRequests: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: tt.status, body: tt.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			code, _, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"})
			if code == 0 {
				t.Fatal("exit code = 0, want failure")
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", stderr, tt.wantStderr)
			}
			if len(fake.requests) != tt.wantRequests {
				t.Errorf("server got %d requests, want %d", len(fake.requests), tt.wantRequests)
			}
		})
	}
}
