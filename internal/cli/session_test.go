package cli

import (
	"crypto/sha256"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
)

func TestRunSessionLifecycle(t *testing.T) {
	t.Parallel()

	r, st := migratedDatabase(t)
	r.env[envActor] = "ops"
	for i, s := range []store.NewSession{
		{Provider: "oidc", Subject: "sub-a", Username: "alice", Role: core.RoleAdmin},
		{Provider: "oidc", Subject: "sub-a", Username: "alice", Role: core.RoleAdmin},
		{Provider: "github", Subject: "583231", Username: "octocat", Role: core.RoleViewer},
	} {
		s.Hash = sha256.Sum256([]byte{byte(i)})
		s.TTL = time.Hour
		if _, err := st.CreateSession(t.Context(), s); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	list, _ := r.mustRun("session", "list")
	assertLines(t, list,
		`^ACTOR\s+USERNAME\s+ROLE\s+CREATED\s+EXPIRES$`,
		`^user:oidc:sub-a\s+alice\s+admin\s+\d{4}-\S+Z\s+\d{4}-\S+Z$`,
		`^user:oidc:sub-a\s+alice\s+admin\s+\d{4}-\S+Z\s+\d{4}-\S+Z$`,
		`^user:github:583231\s+octocat\s+viewer\s+\d{4}-\S+Z\s+\d{4}-\S+Z$`,
	)

	if _, stderr := r.mustRun("session", "revoke", "--user", "Alice"); stderr != "revoked 2 sessions\n" {
		t.Errorf("revoke --user stderr = %q", stderr)
	}
	if _, stderr := r.mustRun("session", "revoke", "--user", "nobody"); stderr != "revoked 0 sessions\n" {
		t.Errorf("revoke unknown user stderr = %q", stderr)
	}
	if _, stderr := r.mustRun("session", "revoke", "--all"); stderr != "revoked 1 session\n" {
		t.Errorf("revoke --all stderr = %q", stderr)
	}
	if list, _ := r.mustRun("session", "list"); strings.Count(list, "\n") != 1 {
		t.Errorf("list after revoke = %q", list)
	}

	entries, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var got []string
	for _, e := range slices.Backward(entries) {
		got = append(got, string(e.Action)+" "+e.Actor.String())
	}
	want := []string{"session.revoke cli:ops", "session.revoke cli:ops", "session.revoke cli:ops"}
	if !slices.Equal(got, want) {
		t.Errorf("audit = %q, want %q", got, want)
	}
	if !strings.Contains(string(entries[2].Detail), `"sessions": 2`) && !strings.Contains(string(entries[2].Detail), `"sessions":2`) {
		t.Errorf("first revoke detail = %s", entries[2].Detail)
	}
}

func TestRunSessionErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "revoke without target", args: []string{"session", "revoke"}, wantErr: "set exactly one of --user and --all"},
		{name: "revoke with both", args: []string{"session", "revoke", "--user", "alice", "--all"}, wantErr: "set exactly one of --user and --all"},
		{name: "revoke with bad actor", args: []string{"session", "revoke", "--all", "--actor", "bad\tname"}, wantErr: "--actor"},
		{name: "no database", args: []string{"session", "list"}, wantErr: "database"},
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
