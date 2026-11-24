package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type cliRun struct {
	t   *testing.T
	env map[string]string
}

func (r cliRun) run(args ...string) (code int, stdout, stderr string) {
	r.t.Helper()
	var out, errOut bytes.Buffer
	code = Run(r.t.Context(), args, &out, &errOut, envFrom(r.env))
	return code, out.String(), errOut.String()
}

func (r cliRun) mustRun(args ...string) (stdout, stderr string) {
	r.t.Helper()
	code, stdout, stderr := r.run(args...)
	if code != 0 {
		r.t.Fatalf("%v: exit code %d, stderr %q", args, code, stderr)
	}
	return stdout, stderr
}

func migratedDatabase(t *testing.T) (cliRun, *store.Store) {
	t.Helper()
	_, env := withDatabase(t, databaseURLEnv, nil, nil)
	r := cliRun{t: t, env: env}
	r.mustRun("migrate")
	st, err := store.Open(t.Context(), env[config.EnvDatabaseURL])
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return r, st
}

var tokenPattern = regexp.MustCompile(`^dsp_[A-Za-z0-9_-]{43}\n$`)

func TestRunTokenLifecycle(t *testing.T) {
	t.Parallel()

	r, st := migratedDatabase(t)

	stdout, stderr := r.mustRun("token", "create", "--name", "ci", "--role", "deployer")
	if !tokenPattern.MatchString(stdout) {
		t.Fatalf("stdout = %q, want one token line", stdout)
	}
	if !strings.Contains(stderr, "created token ci with role deployer, expires never") || strings.Contains(stderr, strings.TrimSpace(stdout)) {
		t.Errorf("stderr = %q", stderr)
	}
	stored, err := st.FindAPITokenByHash(t.Context(), sha256.Sum256([]byte(strings.TrimSpace(stdout))))
	if err != nil || stored.Name != "ci" || stored.Role != core.RoleDeployer {
		t.Fatalf("stored token = %+v, %v", stored, err)
	}
	wantCreator := cliActor(osUsername())
	if stored.CreatedBy != wantCreator.String() {
		t.Errorf("created_by = %q, want %q", stored.CreatedBy, wantCreator)
	}

	expiring, stderr := r.mustRun("token", "create", "--name", "short-lived", "--role", "viewer", "--expires-in", "1h")
	if !tokenPattern.MatchString(expiring) || expiring == stdout || !regexp.MustCompile(`expires \d{4}-\d{2}-\d{2}T`).MatchString(stderr) {
		t.Errorf("expiring token stdout %q stderr %q", expiring, stderr)
	}

	code, _, stderr := r.run("token", "create", "--name", "ci", "--role", "admin")
	if code != 1 || !strings.Contains(stderr, "already exists") {
		t.Errorf("duplicate create = %d %q", code, stderr)
	}

	list, _ := r.mustRun("token", "list")
	assertLines(t, list,
		`^NAME\s+ROLE\s+CREATED\s+EXPIRES\s+LAST USED\s+STATE$`,
		`^ci\s+deployer\s+\d{4}-\S+Z\s+never\s+never\s+active$`,
		`^short-lived\s+viewer\s+\d{4}-\S+Z\s+\d{4}-\S+Z\s+never\s+active$`,
	)

	_, stderr = r.mustRun("token", "revoke", "ci")
	if stderr != "revoked token ci\n" {
		t.Errorf("revoke stderr = %q", stderr)
	}
	code, _, stderr = r.run("token", "revoke", "ci")
	if code != 1 || !strings.Contains(stderr, `no active token named "ci"`) {
		t.Errorf("second revoke = %d %q", code, stderr)
	}
	list, _ = r.mustRun("token", "list")
	if !regexp.MustCompile(`(?m)^ci\s+deployer\s+.*revoked$`).MatchString(list) {
		t.Errorf("list after revoke = %q", list)
	}
	if _, err := st.FindAPITokenByHash(t.Context(), sha256.Sum256([]byte(strings.TrimSpace(stdout)))); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("revoked token still found: %v", err)
	}

	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var actions []core.AuditAction
	for _, e := range slices.Backward(entries) {
		actions = append(actions, e.Action)
		if e.Actor != wantCreator {
			t.Errorf("audit actor = %+v, want %+v", e.Actor, wantCreator)
		}
	}
	if want := []core.AuditAction{core.AuditTokenCreate, core.AuditTokenCreate, core.AuditTokenRevoke}; !slices.Equal(actions, want) {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
}

func TestRunTokenErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "unknown role", args: []string{"token", "create", "--name", "ci", "--role", "root"}, wantErr: `--role: unknown role "root" (want viewer, reporter, deployer, admin)`},
		{name: "bad name", args: []string{"token", "create", "--name", "Bad Name", "--role", "viewer"}, wantErr: "--name: invalid token name"},
		{name: "missing flags", args: []string{"token", "create"}, wantErr: `required flag(s) "name", "role" not set`},
		{name: "negative expiry", args: []string{"token", "create", "--name", "ci", "--role", "viewer", "--expires-in", "-1h"}, wantErr: "--expires-in: must not be negative"},
		{name: "revoke without name", args: []string{"token", "revoke"}, wantErr: "accepts 1 arg"},
		{name: "no database", args: []string{"token", "list"}, wantErr: config.ErrDatabaseSource.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := cliRun{t: t}.run(tt.args...)
			if code != 1 || !strings.Contains(stderr, tt.wantErr) || stdout != "" {
				t.Errorf("exit %d stdout %q stderr %q, want error %q", code, stdout, stderr, tt.wantErr)
			}
		})
	}
}

func TestRunTokenRequiresMigratedSchema(t *testing.T) {
	t.Parallel()

	_, env := withDatabase(t, databaseURLEnv, nil, nil)
	code, stdout, stderr := cliRun{t: t, env: env}.run("token", "create", "--name", "ci", "--role", "viewer")
	if code != 1 || stdout != "" || !strings.Contains(stderr, errSchemaBehind.Error()) {
		t.Errorf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestCLIActor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		username string
		want     string
	}{
		{username: "root", want: "cli:root"},
		{username: `CORP\alice`, want: `cli:CORP\alice`},
		{username: "", want: "cli:unknown"},
		{username: " padded", want: "cli:unknown"},
		{username: "bad\nname", want: "cli:unknown"},
	}
	for _, tt := range tests {
		if got := cliActor(tt.username).String(); got != tt.want {
			t.Errorf("cliActor(%q) = %q, want %q", tt.username, got, tt.want)
		}
	}
}

func TestCLIRequester(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		flag       string
		env        map[string]string
		osUser     string
		wantActor  string
		wantOrigin map[string]string
		wantErr    string
	}{
		{name: "os user", osUser: "nonroot", wantActor: "cli:nonroot", wantOrigin: map[string]string{"os_user": "nonroot"}},
		{name: "env", env: map[string]string{envActor: "alice"}, osUser: "nonroot", wantActor: "cli:alice", wantOrigin: map[string]string{"os_user": "nonroot"}},
		{name: "flag over env", flag: "bob", env: map[string]string{envActor: "alice"}, osUser: "root", wantActor: "cli:bob", wantOrigin: map[string]string{"os_user": "root"}},
		{name: "unknown os user", flag: "bob", wantActor: "cli:bob"},
		{name: "nothing known", wantActor: "cli:unknown"},
		{name: "invalid flag", flag: " bob", wantErr: "--actor"},
		{name: "invalid env", env: map[string]string{envActor: "bad\nname"}, wantErr: envActor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := cliRequester(tt.flag, envFrom(tt.env), tt.osUser)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got.Actor.String() != tt.wantActor || !maps.Equal(got.Origin, tt.wantOrigin) {
				t.Errorf("cliRequester = %+v, %v, want %s %v", got, err, tt.wantActor, tt.wantOrigin)
			}
		})
	}
}

func TestRunTokenRecordsActor(t *testing.T) {
	t.Parallel()

	r, st := migratedDatabase(t)
	r.env[envActor] = "alice"
	r.mustRun("token", "create", "--name", "ci", "--role", "viewer")
	r.mustRun("token", "revoke", "ci", "--actor", "bob")
	if code, _, stderr := r.run("token", "revoke", "ci", "--actor", " padded"); code != 1 || !strings.Contains(stderr, "--actor") {
		t.Errorf("invalid --actor = %d %q", code, stderr)
	}

	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var got []string
	for _, e := range slices.Backward(entries) {
		var detail struct {
			Origin map[string]string `json:"origin"`
		}
		if err := json.Unmarshal(e.Detail, &detail); err != nil {
			t.Fatalf("detail %s: %v", e.Detail, err)
		}
		got = append(got, string(e.Action)+" "+e.Actor.String()+" "+detail.Origin["os_user"])
	}
	want := []string{"token.create cli:alice " + osUsername(), "token.revoke cli:bob " + osUsername()}
	if !slices.Equal(got, want) {
		t.Errorf("audit = %q, want %q", got, want)
	}
}
