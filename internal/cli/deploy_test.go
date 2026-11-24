package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const deploySHA = "3f1c0d9e8b7a6f5e4d3c2b1a09f8e7d6c5b4a392"

func TestDeployCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		status     int
		body       string
		wantBody   string
		wantStdout string
	}{
		{
			name:       "queued",
			args:       []string{"deploy", "blog", deploySHA},
			status:     http.StatusCreated,
			body:       `{"deployment":{"id":42,"app":"blog","state":"queued","commit":{"sha":"` + deploySHA + `"}},"created":true}`,
			wantBody:   `{"commit_sha":"` + deploySHA + `"}`,
			wantStdout: "deployment 42 queued for blog: commit 3f1c0d9e8b7a\n",
		},
		{
			name:   "urgent with a reason and a migration",
			args:   []string{"deploy", "blog", deploySHA, "--urgent", "--reason", " hotfix "},
			status: http.StatusCreated,
			body: `{"deployment":{"id":42,"app":"blog","state":"queued","commit":{"sha":"` + deploySHA +
				`"},"migration":true,"migration_reason":"trailer Doorstep-Migration in 3f1c0d9"},"created":true}`,
			wantBody:   `{"commit_sha":"` + deploySHA + `","urgent":true,"reason":"hotfix"}`,
			wantStdout: "deployment 42 queued for blog: commit 3f1c0d9e8b7a (urgent); flagged as a database migration: trailer Doorstep-Migration in 3f1c0d9\n",
		},
		{
			name:       "uppercase sha is sent lowercase",
			args:       []string{"deploy", "blog", strings.ToUpper(deploySHA)},
			status:     http.StatusCreated,
			body:       `{"deployment":{"id":42,"app":"blog","state":"queued","commit":{"sha":"` + deploySHA + `"}},"created":true}`,
			wantBody:   `{"commit_sha":"` + deploySHA + `"}`,
			wantStdout: "deployment 42 queued for blog: commit 3f1c0d9e8b7a\n",
		},
		{
			name:       "already running",
			args:       []string{"deploy", "blog", deploySHA},
			status:     http.StatusOK,
			body:       `{"deployment":{"id":40,"app":"blog","state":"syncing","commit":{"sha":"` + deploySHA + `"}},"created":false}`,
			wantBody:   `{"commit_sha":"` + deploySHA + `"}`,
			wantStdout: "deployment 40 of commit 3f1c0d9e8b7a is already syncing for blog\n",
		},
		{
			name:       "json",
			args:       []string{"deploy", "blog", deploySHA, "-o", "json"},
			status:     http.StatusCreated,
			body:       `{"created":true}`,
			wantBody:   `{"commit_sha":"` + deploySHA + `"}`,
			wantStdout: "{\n  \"created\": true\n}\n",
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
				t.Errorf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			req := fake.only(t)
			if req.method != http.MethodPost || req.path != "/api/v1/apps/blog/deployments" || req.auth != "Bearer tok-123" || req.body != tt.wantBody {
				t.Errorf("request = %s %s %q (auth %q)", req.method, req.path, req.body, req.auth)
			}
		})
	}
}

func TestCancelCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantBody   string
		wantStdout string
	}{
		{name: "plain", args: []string{"cancel", "42"}, wantBody: `{}`, wantStdout: "deployment 42 of blog cancelled\n"},
		{name: "reason", args: []string{"cancel", "42", "--reason", "wrong commit"}, wantBody: `{"reason":"wrong commit"}`, wantStdout: "deployment 42 of blog cancelled\n"},
		{name: "json", args: []string{"cancel", "42", "-o", "json"}, wantBody: `{}`, wantStdout: "{\n  \"deployment\": {\n    \"id\": 42,\n    \"app\": \"blog\",\n    \"state\": \"cancelled\"\n  }\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: http.StatusOK, body: `{"deployment":{"id":42,"app":"blog","state":"cancelled"}}`}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			code, stdout, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"})
			if code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr)
			}
			if stdout != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			req := fake.only(t)
			if req.method != http.MethodPost || req.path != "/api/v1/deployments/42/cancel" || req.body != tt.wantBody {
				t.Errorf("request = %s %s %q", req.method, req.path, req.body)
			}
		})
	}
}

func TestDeployAndCancelCommandErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		args         []string
		status       int
		body         string
		wantStderr   string
		wantRequests int
	}{
		{name: "deploy without a commit", args: []string{"deploy", "blog"}, wantStderr: "accepts 2 arg"},
		{name: "deploy invalid app", args: []string{"deploy", "Blog!", deploySHA}, wantStderr: "app name"},
		{name: "deploy short sha", args: []string{"deploy", "blog", "3f1c0d9"}, wantStderr: "commit: invalid commit SHA"},
		{name: "deploy bad output", args: []string{"deploy", "blog", deploySHA, "-o", "yaml"}, wantStderr: "--output: must be table or json"},
		{
			name: "deploy not on branch", args: []string{"deploy", "blog", deploySHA}, status: http.StatusConflict,
			body:       `{"error":{"code":"commit_not_on_branch","message":"commit is not on the app's branch: 3f1c0d9e is not reachable from main"}}`,
			wantStderr: "doorstep: commit is not on the app's branch", wantRequests: 1,
		},
		{name: "cancel without id", args: []string{"cancel"}, wantStderr: "accepts 1 arg"},
		{name: "cancel bad id", args: []string{"cancel", "abc"}, wantStderr: "deployment id: must be a positive integer"},
		{name: "cancel zero", args: []string{"cancel", "0"}, wantStderr: "deployment id: must be a positive integer"},
		{
			name: "cancel finished", args: []string{"cancel", "42"}, status: http.StatusConflict,
			body:       `{"error":{"code":"not_cancellable","message":"deployment is not cancellable: deployment 42 is already succeeded"}}`,
			wantStderr: "already succeeded", wantRequests: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: tt.status, body: tt.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()
			code, _, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"})
			if code == 0 || !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("exit code = %d, stderr %q, want failure containing %q", code, stderr, tt.wantStderr)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.requests) != tt.wantRequests {
				t.Errorf("requests = %d, want %d", len(fake.requests), tt.wantRequests)
			}
		})
	}
}
