package cli

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/api"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/metrics"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

func succeededDeployment(t *testing.T, st *store.Store, sha string, digest core.Digest) core.Deployment {
	t.Helper()
	d, _, err := st.EnqueueDeployment(t.Context(), core.Deployment{
		Target:      core.AppTarget("blog"),
		Kind:        core.KindDeploy,
		Source:      core.SourceWebhook,
		RequestedBy: "octocat",
		Commit:      core.Commit{Repository: "example-user/blog", SHA: sha},
		Image:       core.ImageRef{Repository: "ghcr.io/example-user/blog", Tag: sha},
	}, core.PriorityNormal)
	if err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	for _, next := range []core.State{core.StateResolving, core.StatePinning, core.StateSyncing, core.StateVerifying, core.StateSucceeded} {
		tr := store.Transition{ID: d.ID, From: d.State, To: next}
		if next == core.StateResolving {
			tr.Lineage.Digest = digest
		}
		if d, err = st.TransitionDeployment(t.Context(), tr); err != nil {
			t.Fatalf("TransitionDeployment: %v", err)
		}
	}
	return d
}

func TestRollbackCommandsAgainstAPI(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	m := metrics.New(metrics.Options{})
	service, err := rollback.New(rollback.Options{
		Repository: rollback.NewRepository(st),
		Observer:   rollbackMetrics{recorder: m.Recorder()},
	})
	if err != nil {
		t.Fatalf("rollback.New: %v", err)
	}
	handler := api.New(api.Options{
		Rollbacks:     service,
		Blocks:        service,
		KnownApp:      knownApp(oneAppCatalog(t)),
		Authenticator: staticAuth{},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	env := map[string]string{EnvServerURL: srv.URL, EnvToken: "tok-123"}

	good := succeededDeployment(t, st, strings.Repeat("a", 40), cliGoodDigest)
	bad := succeededDeployment(t, st, cliBadSHA, cliBadDigest)

	steps := []struct {
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{args: []string{"blocks", "blog"}, wantStdout: "no blocks for blog\n"},
		{args: []string{"rollback", "blog", "--reason", "checkout errors"}, wantStdout: "queued for blog: ghcr.io/example-user/blog@sha256:bbbbbbbbbbbb (reverting deployment " +
			strconv.FormatInt(int64(bad.ID), 10) + "); blocked " + cliBadDigest + " (commit " + cliBadSHA + ")\n"},
		{args: []string{"rollback", "blog"}, wantCode: 1, wantStderr: "is already queued"},
		{args: []string{"rollback", "other"}, wantCode: 1, wantStderr: "unknown app other"},
		{args: []string{"blocks", "blog"}, wantStdout: "user:alice"},
		{args: []string{"block", "blog", cliBadDigest, "--reason", "again"}, wantStdout: "blocked " + cliBadDigest + " (commit " + cliBadSHA + ") for blog by user:alice: checkout errors\n"},
		{args: []string{"unblock", "blog", cliBadSHA}, wantStdout: "unblocked " + cliBadDigest + " (commit " + cliBadSHA + ") for blog\n"},
		{args: []string{"unblock", "blog", cliBadSHA}, wantCode: 1, wantStderr: "is not blocked for blog"},
		{args: []string{"blocks", "blog", "-o", "json"}, wantStdout: "{\n  \"blocks\": []\n}\n"},
	}
	for _, step := range steps {
		code, stdout, stderr := runCLI(t, step.args, env)
		if code != step.wantCode {
			t.Fatalf("%v: exit code = %d, want %d (stderr %q)", step.args, code, step.wantCode, stderr)
		}
		if !strings.Contains(stdout, step.wantStdout) {
			t.Errorf("%v: stdout %q does not contain %q", step.args, stdout, step.wantStdout)
		}
		if !strings.Contains(stderr, step.wantStderr) {
			t.Errorf("%v: stderr %q does not contain %q", step.args, stderr, step.wantStderr)
		}
	}

	entries, err := st.QueueEntries(t.Context(), "blog")
	if err != nil || len(entries) != 1 || entries[0].Deployment.Kind != core.KindRollback || entries[0].Deployment.Digest != good.Digest {
		t.Fatalf("queue = %+v, %v, want one rollback to %s", entries, err, good.Digest)
	}
	audit, err := st.ListAudit(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var actions []core.AuditAction
	for _, e := range audit {
		actions = append(actions, e.Action)
	}
	want := []core.AuditAction{core.AuditBlockClear, core.AuditRollbackRequest}
	if len(actions) != len(want) || actions[0] != want[0] || actions[1] != want[1] {
		t.Errorf("audit actions = %v, want %v", actions, want)
	}
}
