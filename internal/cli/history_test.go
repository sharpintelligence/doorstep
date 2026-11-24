package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const (
	historyListJSON = `{"items":[` +
		`{"id":9,"app":"blog","environment":"prod","kind":"rollback","state":"succeeded","trigger":{"source":"rollback","requested_by":"user:alice"},` +
		`"commit":{"repository":"acme/blog","sha":"` + cliBadSHA + `"},"image":{"repository":"ghcr.io/acme/blog","tag":"b","digest":"` + cliGoodDigest + `"},` +
		`"rollback_of_id":8,"created_at":"2026-11-23T12:05:00Z","finished_at":"2026-11-23T12:06:00Z"},` +
		`{"id":8,"app":"blog","environment":"prod","kind":"deploy","state":"queued","trigger":{"source":"merge"},` +
		`"commit":{"repository":"acme/blog","sha":"` + cliBadSHA + `"},"image":{"repository":"ghcr.io/acme/blog","tag":"c"},` +
		`"created_at":"2026-11-23T12:00:00Z"}],"next_cursor":"8"}`

	historyDetailJSON = `{"deployment":{"id":42,"app":"blog","environment":"prod","kind":"deploy","state":"failed",` +
		`"trigger":{"source":"manual","requested_by":"user:alice"},` +
		`"commit":{"repository":"acme/blog","sha":"` + cliBadSHA + `"},` +
		`"image":{"repository":"ghcr.io/acme/blog","tag":"c","digest":"` + cliBadDigest + `","previous_digest":"` + cliGoodDigest + `"},` +
		`"gitops":{"repository":"acme/gitops","commit_sha":"` + goodGitOpsSHA + `"},"argocd":{"revision":"` + goodGitOpsSHA + `"},` +
		`"parent_id":41,"superseded_by_id":43,"failure_reason":"sync failed\u001b[31m: degraded\nsecond line",` +
		`"migration":true,"migration_reason":"label db-migration",` +
		`"created_at":"2026-11-23T12:00:00Z","started_at":"2026-11-23T12:01:00Z","finished_at":"2026-11-23T12:10:00Z"},` +
		`"events":[` +
		`{"type":"transition","from":"queued","to":"running","actor":"engine","data":{},"occurred_at":"2026-11-23T12:01:00Z"},` +
		`{"type":"retry","actor":"engine","data":{"retry":{"attempt":1,"max_attempts":3,"summary":"transient: image pull failing"}},"occurred_at":"2026-11-23T12:05:00Z"},` +
		`{"type":"note","message":"note\u0007 text","data":{"x":1},"occurred_at":"2026-11-23T12:06:00Z"},` +
		`{"type":"note","data":{"retry":"bad"},"occurred_at":"2026-11-23T12:07:00Z"}],` +
		`"events_truncated":false,` +
		`"gates":[{"name":"checks","type":"required_checks","phase":"before","status":"passed","message":"all\u001b[2J green","evidence":{},"started_at":"2026-11-23T12:00:30Z","finished_at":"2026-11-23T12:00:40Z"},` +
		`{"name":"errors","type":"prometheus","phase":"after","status":"running","started_at":"2026-11-23T12:09:00Z"}],` +
		`"rollbacks":[{"id":44,"state":"succeeded","image":{"digest":"` + cliGoodDigest + `"},"created_at":"2026-11-23T12:20:00Z"}],` +
		`"retries":[{"id":45,"state":"queued","image":{},"created_at":"2026-11-23T12:30:00Z"}]}`

	goodGitOpsSHA = "dddddddddddddddddddddddddddddddddddddddd"

	historyMinimalJSON = `{"deployment":{"id":7,"app":"blog","kind":"deploy","state":"queued","trigger":{"source":"merge"},"commit":{},"image":{},"created_at":"2026-11-23T12:00:00Z"},` +
		`"events":[],"events_truncated":false,"gates":[],"rollbacks":[],"retries":[]}`
)

func TestDeploymentsCommand(t *testing.T) {
	t.Parallel()

	const listTable = "ID  ENVIRONMENT  KIND      STATE      COMMIT        DIGEST               SOURCE    BY          CREATED               FINISHED\n" +
		"9   prod         rollback  succeeded  cccccccccccc  sha256:bbbbbbbbbbbb  rollback  user:alice  2026-11-23T12:05:00Z  2026-11-23T12:06:00Z\n" +
		"8   prod         deploy    queued     cccccccccccc  -                    merge     -           2026-11-23T12:00:00Z  -\n"

	tests := []struct {
		name       string
		args       []string
		body       string
		wantQuery  url.Values
		wantStdout string
		wantStderr string
	}{
		{
			name:       "default request",
			args:       []string{"deployments", "blog"},
			body:       historyListJSON,
			wantQuery:  url.Values{"limit": {"50"}},
			wantStdout: listTable,
			wantStderr: "more deployments: rerun with --cursor 8\n",
		},
		{
			name: "every filter",
			args: []string{
				"deployments", "blog", "--environment", "prod", "--state", "succeeded,failed", "--state", "queued",
				"--kind", "rollback", "--kind", "deploy", "--commit", strings.ToUpper(cliBadSHA), "--digest", cliBadDigest,
				"--limit", "5", "--cursor", "30",
			},
			body: `{"items":[]}`,
			wantQuery: url.Values{
				"environment": {"prod"}, "state": {"succeeded", "failed", "queued"}, "kind": {"rollback", "deploy"},
				"commit": {cliBadSHA}, "digest": {cliBadDigest}, "limit": {"5"}, "cursor": {"30"},
			},
			wantStdout: "no deployments found for blog\n",
		},
		{
			name:       "environment with reserved characters is one parameter",
			args:       []string{"deployments", "blog", "--environment", "a&limit=1"},
			body:       `{"items":[]}`,
			wantQuery:  url.Values{"environment": {"a&limit=1"}, "limit": {"50"}},
			wantStdout: "no deployments found for blog\n",
		},
		{
			name:       "json passes next_cursor through",
			args:       []string{"deployments", "blog", "-o", "json"},
			body:       `{"items":[],"next_cursor":"8"}`,
			wantQuery:  url.Values{"limit": {"50"}},
			wantStdout: "{\n  \"items\": [],\n  \"next_cursor\": \"8\"\n}\n",
		},
		{
			name:       "last page prints no hint",
			args:       []string{"deployments", "blog"},
			body:       `{"items":[]}`,
			wantQuery:  url.Values{"limit": {"50"}},
			wantStdout: "no deployments found for blog\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: http.StatusOK, body: tt.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			code, stdout, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"})
			if code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr)
			}
			if stdout != tt.wantStdout {
				t.Errorf("stdout =\n%s\nwant\n%s", stdout, tt.wantStdout)
			}
			if stderr != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr, tt.wantStderr)
			}
			req := fake.only(t)
			if req.method != http.MethodGet || req.path != "/api/v1/apps/blog/deployments" || req.auth != "Bearer tok-123" {
				t.Errorf("request = %s %s (auth %q), want GET /api/v1/apps/blog/deployments", req.method, req.path, req.auth)
			}
			gotQuery, err := url.ParseQuery(req.query)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotQuery, tt.wantQuery) {
				t.Errorf("query = %v, want %v", gotQuery, tt.wantQuery)
			}
		})
	}
}

func TestDeploymentCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		body       string
		wantStdout string
		wantParts  []string
	}{
		{name: "full lineage", args: []string{"deployment", "42"}, body: historyDetailJSON, wantParts: []string{
			"deployment 42: deploy of blog (prod) failed\n",
			"  digest:           " + cliBadDigest + "\n",
			"  previous digest:  " + cliGoodDigest + "\n",
			"  failure:          sync failed [31m: degraded second line\n",
			"\nGATES\n", "all [2J green", "\nEVENTS\n", "retry 1/3: transient: image pull failing", "note  text",
			"\nROLLBACKS\n", "\nRETRIES\n",
		}},
		{
			name: "minimal queued deployment", args: []string{"deployment", "7"}, body: historyMinimalJSON,
			wantStdout: "deployment 7: deploy of blog queued\n  trigger:  merge\n  created:  2026-11-23T12:00:00Z\n",
		},
		{
			name: "truncated events", args: []string{"deployment", "7"},
			body:      strings.Replace(strings.Replace(historyMinimalJSON, `"events":[]`, `"events":[{"type":"note","message":"m","occurred_at":"2026-11-23T12:00:00Z"}]`, 1), `"events_truncated":false`, `"events_truncated":true`, 1),
			wantParts: []string{"\nEVENTS (older events omitted)\n"},
		},
		{
			name: "json passthrough", args: []string{"deployment", "7", "-o", "json"}, body: `{"deployment":{"id":7}}`,
			wantStdout: "{\n  \"deployment\": {\n    \"id\": 7\n  }\n}\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: http.StatusOK, body: tt.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			code, stdout, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"})
			if code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr)
			}
			if tt.wantStdout != "" && stdout != tt.wantStdout {
				t.Errorf("stdout =\n%q\nwant\n%q", stdout, tt.wantStdout)
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(stdout, part) {
					t.Errorf("stdout =\n%s\nmissing %q", stdout, part)
				}
			}
			if strings.ContainsAny(stdout, "\x1b\x07") {
				t.Errorf("stdout contains control characters: %q", stdout)
			}
			req := fake.only(t)
			if req.method != http.MethodGet || req.path != "/api/v1/deployments/"+tt.args[1] || req.query != "" || req.auth != "Bearer tok-123" {
				t.Errorf("request = %+v, want GET /api/v1/deployments/%s without query", req, tt.args[1])
			}
		})
	}
}

func TestDeploymentDetailTable(t *testing.T) {
	t.Parallel()

	fake := &fakeAPI{status: http.StatusOK, body: historyDetailJSON}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	code, stdout, stderr := runCLI(t, []string{"deployment", "42"}, map[string]string{EnvServerURL: srv.URL})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr)
	}
	for _, line := range []string{
		"deployment 42: deploy of blog (prod) failed",
		"  trigger:          manual by user:alice",
		"  commit:           acme/blog@" + cliBadSHA,
		"  image:            ghcr.io/acme/blog:c",
		"  gitops:           acme/gitops@" + goodGitOpsSHA,
		"  retry of:         41",
		"  superseded by:    43",
		"  migration:        yes: label db-migration",
		"checks  before  required_checks  passed   2026-11-23T12:00:40Z  all [2J green",
		"errors  after   prometheus       running  -                     -",
		"2026-11-23T12:01:00Z  transition  queued -> running  engine  -",
		"44  succeeded  sha256:bbbbbbbbbbbb  2026-11-23T12:20:00Z",
		"45  queued  -       2026-11-23T12:30:00Z",
	} {
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("stdout =\n%s\nmissing line %q", stdout, line)
		}
	}
	sections := []string{"\nGATES\n", "\nEVENTS\n", "\nROLLBACKS\n", "\nRETRIES\n"}
	last := -1
	for _, s := range sections {
		i := strings.Index(stdout, s)
		if i <= last {
			t.Errorf("section %q is missing or out of order in\n%s", s, stdout)
		}
		last = i
	}
}

func TestHistoryCommandsRejectBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{name: "bad app name", args: []string{"deployments", "a/b"}, wantStderr: "app name"},
		{name: "bad output", args: []string{"deployments", "blog", "-o", "yaml"}, wantStderr: "--output: must be table or json"},
		{name: "limit zero", args: []string{"deployments", "blog", "--limit", "0"}, wantStderr: "--limit: must be from 1 to 200"},
		{name: "limit too large", args: []string{"deployments", "blog", "--limit", "201"}, wantStderr: "--limit: must be from 1 to 200"},
		{name: "short commit", args: []string{"deployments", "blog", "--commit", "abc"}, wantStderr: "--commit:"},
		{name: "bad digest", args: []string{"deployments", "blog", "--digest", "sha256:abc"}, wantStderr: "--digest:"},
		{name: "unknown state", args: []string{"deployments", "blog", "--state", "exploded"}, wantStderr: "--state:"},
		{name: "unknown state in list", args: []string{"deployments", "blog", "--state", "queued,exploded"}, wantStderr: "--state:"},
		{name: "unknown kind", args: []string{"deployments", "blog", "--kind", "party"}, wantStderr: `--kind: unknown kind "party"`},
		{name: "blank environment", args: []string{"deployments", "blog", "--environment", "  "}, wantStderr: "--environment: must not be blank"},
		{name: "cursor text", args: []string{"deployments", "blog", "--cursor", "abc"}, wantStderr: "--cursor: must be a positive integer"},
		{name: "cursor zero", args: []string{"deployments", "blog", "--cursor", "0"}, wantStderr: "--cursor: must be a positive integer"},
		{name: "cursor negative", args: []string{"deployments", "blog", "--cursor", "-1"}, wantStderr: "--cursor: must be a positive integer"},
		{name: "cursor not canonical", args: []string{"deployments", "blog", "--cursor", "+5"}, wantStderr: "--cursor: must be a positive integer"},
		{name: "deployments without app", args: []string{"deployments"}, wantStderr: "accepts 1 arg"},
		{name: "deployments with two apps", args: []string{"deployments", "a", "b"}, wantStderr: "accepts 1 arg"},
		{name: "deployment id zero", args: []string{"deployment", "0"}, wantStderr: `deployment id: must be a positive integer, got "0"`},
		{name: "deployment id text", args: []string{"deployment", "x"}, wantStderr: `deployment id: must be a positive integer, got "x"`},
		{name: "deployment id signed", args: []string{"deployment", "+1x"}, wantStderr: "deployment id: must be a positive integer"},
		{name: "deployment bad output", args: []string{"deployment", "1", "-o", "yaml"}, wantStderr: "--output: must be table or json"},
		{name: "deployment without id", args: []string{"deployment"}, wantStderr: "accepts 1 arg"},
		{name: "deployment with two ids", args: []string{"deployment", "1", "2"}, wantStderr: "accepts 1 arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: http.StatusOK, body: `{}`}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			code, stdout, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL})
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.requests) != 0 {
				t.Errorf("server got %d requests, want none: %+v", len(fake.requests), fake.requests)
			}
		})
	}
}

func TestHistoryCommandsReportAPIErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		status     int
		body       string
		wantStderr string
	}{
		{
			name: "bad request", args: []string{"deployments", "blog"}, status: http.StatusBadRequest,
			body: `{"error":{"code":"invalid_request","message":"unknown query parameter"}}`, wantStderr: "doorstep: unknown query parameter\n",
		},
		{
			name: "not found", args: []string{"deployment", "42"}, status: http.StatusNotFound,
			body: `{"error":{"code":"not_found","message":"deployment 42 not found"}}`, wantStderr: "doorstep: deployment 42 not found\n",
		},
		{
			name: "unauthorized without token", args: []string{"deployments", "blog"}, status: http.StatusUnauthorized,
			body: `{"error":{"code":"unauthorized","message":"authentication required"}}`, wantStderr: "authentication required (set " + EnvToken,
		},
		{
			name: "malformed list", args: []string{"deployments", "blog"}, status: http.StatusOK,
			body: `{"items":`, wantStderr: "decode response",
		},
		{
			name: "malformed detail", args: []string{"deployment", "42"}, status: http.StatusOK,
			body: `[1]`, wantStderr: "decode response",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: tt.status, body: tt.body}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			code, stdout, stderr := runCLI(t, tt.args, map[string]string{EnvServerURL: srv.URL})
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestPrintable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "all good", want: "all good"},
		{name: "unicode kept", in: "déploiement ✓", want: "déploiement ✓"},
		{name: "escape sequence", in: "a\x1b[31mred", want: "a [31mred"},
		{name: "newline carriage return tab", in: "a\nb\rc\td", want: "a b c d"},
		{name: "c1 control", in: "a\u009bb", want: "a b"},
		{name: "nul and bell", in: "a\x00b\x07", want: "a b "},
		{name: "empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := printable(tt.in); got != tt.want {
				t.Errorf("printable(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseDeploymentID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1", want: 1},
		{in: "42", want: 42},
		{in: "9223372036854775807", want: 9223372036854775807},
		{in: "0", wantErr: true},
		{in: "-3", wantErr: true},
		{in: "x", wantErr: true},
		{in: "", wantErr: true},
		{in: "1.5", wantErr: true},
		{in: "9223372036854775808", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := parseDeploymentID(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("parseDeploymentID(%q) = %d, %v; want %d, error %v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestAPIClientRequestEncodesQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		prefix    string
		query     url.Values
		wantPath  string
		wantQuery url.Values
	}{
		{name: "no query", wantPath: "/api/v1/x", wantQuery: url.Values{}},
		{name: "reserved characters stay in one value", query: url.Values{"a": {"b&c=d"}}, wantPath: "/api/v1/x", wantQuery: url.Values{"a": {"b&c=d"}}},
		{name: "repeated values", query: url.Values{"s": {"1", "2"}}, wantPath: "/api/v1/x", wantQuery: url.Values{"s": {"1", "2"}}},
		{name: "base path prefix", prefix: "/doorstep", query: url.Values{"a": {"1"}}, wantPath: "/doorstep/api/v1/x", wantQuery: url.Values{"a": {"1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{status: http.StatusNoContent}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			client, err := newAPIClient(func(string) bool { return false }, "", envFrom(map[string]string{EnvServerURL: srv.URL + tt.prefix}))
			if err != nil {
				t.Fatal(err)
			}
			if err := client.request(t.Context(), http.MethodGet, "/api/v1/x", tt.query, nil, nil); err != nil {
				t.Fatal(err)
			}
			req := fake.only(t)
			gotQuery, err := url.ParseQuery(req.query)
			if err != nil {
				t.Fatal(err)
			}
			if req.path != tt.wantPath || !reflect.DeepEqual(gotQuery, tt.wantQuery) {
				t.Errorf("request = %s ? %v, want %s ? %v", req.path, gotQuery, tt.wantPath, tt.wantQuery)
			}
		})
	}
}
