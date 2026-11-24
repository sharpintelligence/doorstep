package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	cliBadDigest  = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	cliGoodDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cliBadSHA     = "cccccccccccccccccccccccccccccccccccccccc"

	blockJSON = `{"id":3,"app":"blog","environment":"default","digest":"` + cliBadDigest + `","commit":{"repository":"acme/blog","sha":"` + cliBadSHA +
		`"},"reason":"checkout errors","created_by":"user:alice","created_at":"2026-11-23T12:00:00Z","recorded_in_git":false}`
	rollbackJSON = `{"deployment":{"id":9,"app":"blog","kind":"rollback","state":"queued","image":{"repository":"ghcr.io/acme/blog","tag":"b","digest":"` + cliGoodDigest +
		`"}},"rolled_back":{"id":7,"app":"blog","kind":"deploy","state":"cancelled","image":{"repository":"ghcr.io/acme/blog","tag":"c"}},"block":` + blockJSON +
		`,"cancelled":[7,8]}`
)

func TestRollbackCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		body       string
		wantBody   string
		wantStdout string
	}{
		{
			name:     "one click",
			args:     []string{"rollback", "blog"},
			body:     rollbackJSON,
			wantBody: `{}`,
			wantStdout: "rollback 9 queued for blog: ghcr.io/acme/blog@sha256:bbbbbbbbbbbb (reverting deployment 7); blocked " +
				cliBadDigest + " (commit " + cliBadSHA + "); cancelled 7, 8\n",
		},
		{
			name:       "explicit target and reason",
			args:       []string{"rollback", "blog", "--to", "41", "--reason", " checkout errors "},
			body:       `{"deployment":{"id":9,"image":{"repository":"ghcr.io/acme/blog","digest":"` + cliGoodDigest + `"}},"rolled_back":{"id":7},"block":null,"cancelled":[]}`,
			wantBody:   `{"to_deployment_id":41,"reason":"checkout errors"}`,
			wantStdout: "rollback 9 queued for blog: ghcr.io/acme/blog@sha256:bbbbbbbbbbbb (reverting deployment 7)\n",
		},
		{
			name:       "json output",
			args:       []string{"rollback", "blog", "-o", "json"},
			body:       `{"deployment":{"id":9},"cancelled":[]}`,
			wantBody:   `{}`,
			wantStdout: "{\n  \"deployment\": {\n    \"id\": 9\n  },\n  \"cancelled\": []\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: http.StatusCreated, body: tt.body}
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
			if req.method != http.MethodPost || req.path != "/api/v1/apps/blog/rollback" || req.auth != "Bearer tok-123" {
				t.Errorf("request = %s %s (auth %q)", req.method, req.path, req.auth)
			}
			if req.body != tt.wantBody || req.contentType != "application/json" {
				t.Errorf("body = %q (%s), want %q", req.body, req.contentType, tt.wantBody)
			}
		})
	}
}

func TestRollbackCommandErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		args         []string
		status       int
		body         string
		wantStderr   string
		wantRequests int
	}{
		{name: "no app", args: []string{"rollback"}, wantStderr: "accepts 1 arg"},
		{name: "invalid app", args: []string{"rollback", "Blog!"}, wantStderr: "app name"},
		{name: "bad target", args: []string{"rollback", "blog", "--to", "0"}, wantStderr: "--to: must be a positive deployment id"},
		{name: "bad output", args: []string{"rollback", "blog", "-o", "yaml"}, wantStderr: `--output: must be table or json, got "yaml"`},
		{
			name:         "nothing to roll back to",
			args:         []string{"rollback", "blog"},
			status:       http.StatusConflict,
			body:         `{"error":{"code":"no_rollback_target","message":"no deployment to roll back to: nothing has been deployed to blog/default"}}`,
			wantStderr:   "doorstep: no deployment to roll back to",
			wantRequests: 1,
		},
		{
			name:         "already rolling back",
			args:         []string{"rollback", "blog"},
			status:       http.StatusConflict,
			body:         `{"error":{"code":"rollback_in_progress","message":"rollback 9 of blog/default is already queued"},"deployment":{"id":9}}`,
			wantStderr:   "doorstep: rollback 9 of blog/default is already queued",
			wantRequests: 1,
		},
		{
			name:         "forbidden",
			args:         []string{"rollback", "blog"},
			status:       http.StatusForbidden,
			body:         `{"error":{"code":"forbidden","message":"your role does not allow this"}}`,
			wantStderr:   "doorstep: your role does not allow this",
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

func TestRollbackCommandRefusesPlainHTTPToken(t *testing.T) {
	t.Parallel()

	code, _, stderr := runCLI(t, []string{"rollback", "blog"}, map[string]string{EnvServerURL: "http://doorstep.example", EnvToken: "tok"})
	if code == 0 || !strings.Contains(stderr, "refusing to send the API token over plain http") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
