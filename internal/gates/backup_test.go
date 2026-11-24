package gates_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/config"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/gates"
	"github.com/sharpintelligence/doorstep/internal/metrics"
)

const backupSecret = "backup-signing-secret"

type backupCall struct {
	header http.Header
	body   []byte
}

type backupReceiver struct {
	mu      sync.Mutex
	calls   []backupCall
	respond func(w http.ResponseWriter, call int)
}

func (b *backupReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.calls = append(b.calls, backupCall{header: r.Header.Clone(), body: body})
	n := len(b.calls)
	respond := b.respond
	b.mu.Unlock()
	if respond == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	respond(w, n)
}

func (b *backupReceiver) received() []backupCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backupCall(nil), b.calls...)
}

func answer(status int, body string) func(http.ResponseWriter, int) {
	return func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func fakeSign(secret, body []byte) string {
	return "sha256=" + string(secret) + ":" + string(rune('0'+len(body)%10))
}

type backupFixture struct {
	clock    *fakeClock
	receiver *backupReceiver
	server   *httptest.Server
	secrets  map[string]string
	deps     gates.BackupDeps
}

func newBackupFixture(t *testing.T) *backupFixture {
	t.Helper()
	f := &backupFixture{clock: newClock(), receiver: &backupReceiver{}}
	f.server = httptest.NewServer(f.receiver)
	t.Cleanup(f.server.Close)
	f.secrets = map[string]string{"BACKUP_URL": f.server.URL + "/hooks/backup?token=hidden", "BACKUP_SECRET": backupSecret}
	client := f.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	f.deps = gates.BackupDeps{
		Client: client,
		ReadSecret: func(ref config.SecretRef) (string, error) {
			if ref.Value != "" {
				return ref.Value, nil
			}
			v, ok := f.secrets[ref.Env]
			if !ok {
				return "", errors.New(ref.Label + ": " + ref.Env + " is not set")
			}
			return v, nil
		},
		Sign:      fakeSign,
		Now:       f.clock.Now,
		UserAgent: "doorstep/test",
	}
	return f
}

func backupConfig(mutate ...func(*config.Gate)) config.Gate {
	g := config.Gate{
		Type:    config.GateBackup,
		Name:    "backup",
		When:    config.GateWhenMigration,
		Timeout: new(config.Duration(30 * time.Minute)),
		Webhook: &config.GateWebhook{URLEnv: "BACKUP_URL", SecretEnv: "BACKUP_SECRET"},
	}
	for _, m := range mutate {
		m(&g)
	}
	return g
}

func (f *backupFixture) plan(t *testing.T, g config.Gate) gates.Binding {
	t.Helper()
	r := gates.NewRegistry()
	if err := gates.RegisterBackup(r, f.deps); err != nil {
		t.Fatalf("RegisterBackup: %v", err)
	}
	app := webApp()
	app.Gates = config.Gates{Before: []config.Gate{g}, After: []config.Gate{}}
	plan, err := r.Plan(app)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan.Before[0]
}

func (f *backupFixture) gate(t *testing.T, mutate ...func(*config.Gate)) gates.Gate {
	t.Helper()
	return f.plan(t, backupConfig(mutate...)).Gate
}

func migrationDeployment(kind core.Kind) core.Deployment {
	d := deployment(kind)
	d.Image = core.ImageRef{Repository: "ghcr.io/acme/web", Tag: "v2"}
	d.Digest = core.Digest("sha256:" + strings.Repeat("a", 64))
	d.Migration = true
	d.MigrationReason = "label db-migration on #12 (0123456)"
	return d
}

func TestRegisterBackupValidatesDependencies(t *testing.T) {
	t.Parallel()

	client := &http.Client{}
	read := func(config.SecretRef) (string, error) { return "", nil }
	tests := []struct {
		name    string
		reg     *gates.Registry
		deps    gates.BackupDeps
		wantErr []string
	}{
		{name: "complete", reg: gates.NewRegistry(), deps: gates.BackupDeps{Client: client, ReadSecret: read, Sign: fakeSign}},
		{name: "no registry", deps: gates.BackupDeps{Client: client, ReadSecret: read, Sign: fakeSign}, wantErr: []string{"no registry"}},
		{name: "nothing", reg: gates.NewRegistry(), wantErr: []string{"HTTP client", "secret reader", "signer"}},
		{name: "no signer", reg: gates.NewRegistry(), deps: gates.BackupDeps{Client: client, ReadSecret: read}, wantErr: []string{"signer"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := gates.RegisterBackup(tt.reg, tt.deps)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("RegisterBackup: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RegisterBackup succeeded, want an error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestRegisterBackupTwice(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	r := gates.NewRegistry()
	if err := gates.RegisterBackup(r, f.deps); err != nil {
		t.Fatalf("RegisterBackup: %v", err)
	}
	if err := gates.RegisterBackup(r, f.deps); !errors.Is(err, gates.ErrDuplicateGateType) {
		t.Errorf("second RegisterBackup = %v, want ErrDuplicateGateType", err)
	}
}

func TestBackupBinding(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	b := f.plan(t, backupConfig())
	if !b.Deferred || b.Kind != metrics.GateKindBackup || b.SkipOnRollback || b.Timeout != 30*time.Minute || b.Phase != core.GatePhaseBefore {
		t.Errorf("binding = %+v, want a deferred backup gate with a 30m timeout that decides rollbacks itself", b)
	}
}

func TestBackupFactoryRejectsBadConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*config.Gate)
		want   string
	}{
		{name: "no webhook", mutate: func(g *config.Gate) { g.Webhook = nil }, want: "has no webhook"},
		{name: "unknown when", mutate: func(g *config.Gate) { g.When = "sometimes" }, want: `unknown when "sometimes"`},
		{name: "no secret", mutate: func(g *config.Gate) { g.Webhook = &config.GateWebhook{URLEnv: "BACKUP_URL"} }, want: "has no webhook secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBackupFixture(t)
			r := gates.NewRegistry()
			if err := gates.RegisterBackup(r, f.deps); err != nil {
				t.Fatalf("RegisterBackup: %v", err)
			}
			app := webApp()
			app.Gates = config.Gates{Before: []config.Gate{backupConfig(tt.mutate)}}
			if _, err := r.Plan(app); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Plan error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBackupGateSkips(t *testing.T) {
	t.Parallel()

	unflagged := deployment(core.KindDeploy)
	flaggedRollback := migrationDeployment(core.KindRollback)
	unflaggedRollback := deployment(core.KindRollback)
	tests := []struct {
		name     string
		mutate   func(*config.Gate)
		d        core.Deployment
		want     core.GateStatus
		wantMsg  string
		wantPost bool
	}{
		{name: "unflagged deploy", d: unflagged, want: core.GateSkipped, wantMsg: "release is not flagged as a database migration"},
		{name: "unflagged deploy with empty when", mutate: func(g *config.Gate) { g.When = "" }, d: unflagged, want: core.GateSkipped, wantMsg: "not flagged"},
		{name: "unflagged deploy always", mutate: func(g *config.Gate) { g.When = config.GateWhenAlways }, d: unflagged, want: core.GatePassed, wantPost: true},
		{name: "flagged deploy", d: migrationDeployment(core.KindDeploy), want: core.GatePassed, wantPost: true},
		{name: "flagged rollback", d: flaggedRollback, want: core.GateSkipped, wantMsg: "skipped for rollback"},
		{name: "flagged rollback applied", mutate: func(g *config.Gate) { g.ApplyToRollbacks = true }, d: flaggedRollback, want: core.GatePassed, wantPost: true},
		{name: "unflagged rollback applied", mutate: func(g *config.Gate) { g.ApplyToRollbacks = true }, d: unflaggedRollback, want: core.GateSkipped, wantMsg: "not flagged"},
		{name: "unflagged rollback applied always", mutate: func(g *config.Gate) {
			g.ApplyToRollbacks = true
			g.When = config.GateWhenAlways
		}, d: unflaggedRollback, want: core.GatePassed, wantPost: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBackupFixture(t)
			var mutate []func(*config.Gate)
			if tt.mutate != nil {
				mutate = append(mutate, tt.mutate)
			}
			ev := evaluateGate(t, f.gate(t, mutate...), tt.d, nil)
			if ev.Status != tt.want || !strings.Contains(ev.Message, tt.wantMsg) {
				t.Errorf("evaluation = %s %q, want %s %q", ev.Status, ev.Message, tt.want, tt.wantMsg)
			}
			if posted := len(f.receiver.received()) > 0; posted != tt.wantPost {
				t.Errorf("posted = %v, want %v", posted, tt.wantPost)
			}
		})
	}
}

func TestBackupGateVerdicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		want    core.GateStatus
		wantMsg string
		state   string
	}{
		{name: "200 empty", status: http.StatusOK, want: core.GatePassed, wantMsg: "backup completed"},
		{name: "204", status: http.StatusNoContent, want: core.GatePassed, wantMsg: "backup completed"},
		{name: "200 succeeded with reference", status: http.StatusOK, body: `{"status":"succeeded","message":"snapshot taken","reference":"snap-42"}`, want: core.GatePassed, wantMsg: "backup completed: snapshot taken (snap-42)", state: "succeeded"},
		{name: "200 status case-insensitive", status: http.StatusOK, body: `{"status":" Succeeded "}`, want: core.GatePassed, state: "succeeded"},
		{name: "200 running", status: http.StatusOK, body: `{"status":"running"}`, want: core.GatePending, wantMsg: "backup running", state: "running"},
		{name: "202", status: http.StatusAccepted, want: core.GatePending, wantMsg: "backup running"},
		{name: "202 succeeded", status: http.StatusAccepted, body: `{"status":"succeeded"}`, want: core.GatePassed, state: "succeeded"},
		{name: "200 failed", status: http.StatusOK, body: `{"status":"failed","message":"disk full"}`, want: core.GateFailed, wantMsg: "backup failed (HTTP 200): disk full", state: "failed"},
		{name: "500 failed body", status: http.StatusInternalServerError, body: `{"status":"failed"}`, want: core.GateFailed, wantMsg: "backup failed (HTTP 500)", state: "failed"},
		{name: "200 unknown status", status: http.StatusOK, body: `{"status":"queued"}`, want: core.GatePassed},
		{name: "200 plain text", status: http.StatusOK, body: "ok\nthanks", want: core.GatePassed, wantMsg: "backup completed: ok thanks"},
		{name: "302", status: http.StatusFound, want: core.GateFailed, wantMsg: "redirects are not followed"},
		{name: "400", status: http.StatusBadRequest, body: `{"message":"unknown app"}`, want: core.GateFailed, wantMsg: "backup webhook rejected the request (HTTP 400): unknown app"},
		{name: "401", status: http.StatusUnauthorized, want: core.GateFailed, wantMsg: "HTTP 401"},
		{name: "404", status: http.StatusNotFound, want: core.GateFailed, wantMsg: "HTTP 404"},
		{name: "408", status: http.StatusRequestTimeout, want: core.GatePending, wantMsg: "HTTP 408; retrying"},
		{name: "425", status: http.StatusTooEarly, want: core.GatePending, wantMsg: "HTTP 425; retrying"},
		{name: "429", status: http.StatusTooManyRequests, want: core.GatePending, wantMsg: "HTTP 429; retrying"},
		{name: "500", status: http.StatusInternalServerError, want: core.GatePending, wantMsg: "HTTP 500; retrying"},
		{name: "503 running", status: http.StatusServiceUnavailable, body: `{"status":"running"}`, want: core.GatePending, wantMsg: "HTTP 503; retrying", state: "running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBackupFixture(t)
			f.receiver.respond = answer(tt.status, tt.body)
			ev := evaluateGate(t, f.gate(t), migrationDeployment(core.KindDeploy), nil)
			if ev.Status != tt.want {
				t.Errorf("status = %s (%q), want %s", ev.Status, ev.Message, tt.want)
			}
			if !strings.Contains(ev.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", ev.Message, tt.wantMsg)
			}
			evidence := evidenceMap(t, ev)
			if evidence["status_code"] != float64(tt.status) || evidence["requests"] != float64(1) {
				t.Errorf("evidence = %v", evidence)
			}
			if got, _ := evidence["state"].(string); got != tt.state {
				t.Errorf("evidence state = %q, want %q", got, tt.state)
			}
			if strings.Contains(string(ev.Evidence), "hidden") || strings.Contains(ev.Message, "hidden") {
				t.Errorf("evaluation leaks the webhook URL: %q %s", ev.Message, ev.Evidence)
			}
		})
	}
}

func TestBackupGateRequest(t *testing.T) {
	t.Parallel()

	f := newBackupFixture(t)
	f.receiver.respond = answer(http.StatusAccepted, "")
	g := f.gate(t)
	d := migrationDeployment(core.KindDeploy)
	first := evaluateGate(t, g, d, nil)
	f.clock.Advance(gates.BackupPollInterval)
	evaluateGate(t, g, d, resultOf(first))

	calls := f.receiver.received()
	if len(calls) != 2 {
		t.Fatalf("received %d calls, want 2", len(calls))
	}
	for i, call := range calls {
		h := call.header
		if h.Get("Content-Type") != "application/json" || h.Get("User-Agent") != "doorstep/test" || h.Get(gates.HeaderBackupEvent) != "backup" {
			t.Errorf("call %d headers = %v", i, h)
		}
		if got := h.Get(gates.HeaderIdempotencyKey); got != "doorstep-backup-7-backup-2026-11-23T12:00:00Z" {
			t.Errorf("call %d idempotency key = %q", i, got)
		}
		sig := h.Get(gates.HeaderBackupSignature)
		if want := fakeSign([]byte(backupSecret), call.body); sig != want {
			t.Errorf("call %d signature = %q, want %q", i, sig, want)
		}
	}
	if string(calls[0].body) != string(calls[1].body) {
		t.Errorf("polls re-posted a different request:\n%s\n%s", calls[0].body, calls[1].body)
	}
	var body map[string]any
	if err := json.Unmarshal(calls[0].body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	dep := body["deployment"].(map[string]any)
	if body["event"] != gates.BackupEvent || body["gate"] != "backup" || body["requested_at"] != "2026-11-23T12:00:00Z" {
		t.Errorf("body = %v", body)
	}
	if dep["id"] != float64(7) || dep["app"] != "web" || dep["environment"] != "prod" || dep["kind"] != "deploy" ||
		dep["migration"] != true || dep["migration_reason"] != d.MigrationReason {
		t.Errorf("deployment = %v", dep)
	}
	if c := dep["commit"].(map[string]any); c["sha"] != d.Commit.SHA || c["repository"] != "acme/web" {
		t.Errorf("commit = %v", c)
	}
	if img := dep["image"].(map[string]any); img["tag"] != "v2" || img["digest"] != string(d.Digest) {
		t.Errorf("image = %v", img)
	}
}

func TestBackupGateThrottlesPolls(t *testing.T) {
	t.Parallel()

	f := newBackupFixture(t)
	f.receiver.respond = func(w http.ResponseWriter, call int) {
		if call < 3 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	g := f.gate(t)
	d := migrationDeployment(core.KindDeploy)

	ev := evaluateGate(t, g, d, nil)
	steps := []struct {
		advance   time.Duration
		wantCalls int
		want      core.GateStatus
	}{
		{advance: time.Second, wantCalls: 1, want: core.GatePending},
		{advance: 13 * time.Second, wantCalls: 1, want: core.GatePending},
		{advance: time.Second, wantCalls: 2, want: core.GatePending},
		{advance: 5 * time.Second, wantCalls: 2, want: core.GatePending},
		{advance: 10 * time.Second, wantCalls: 3, want: core.GatePassed},
	}
	for i, step := range steps {
		f.clock.Advance(step.advance)
		ev = evaluateGate(t, g, d, resultOf(ev))
		if n := len(f.receiver.received()); n != step.wantCalls || ev.Status != step.want {
			t.Fatalf("step %d: %d calls, status %s; want %d calls, %s", i, n, ev.Status, step.wantCalls, step.want)
		}
	}
	evidence := evidenceMap(t, ev)
	if evidence["requests"] != float64(3) || evidence["requested_at"] != "2026-11-23T12:00:00Z" || evidence["last_request_at"] != "2026-11-23T12:00:30Z" {
		t.Errorf("evidence = %v", evidence)
	}
}

func TestBackupGateStartsOverAfterTerminalResult(t *testing.T) {
	t.Parallel()

	f := newBackupFixture(t)
	g := f.gate(t)
	d := migrationDeployment(core.KindDeploy)
	first := evaluateGate(t, g, d, nil)
	f.clock.Advance(time.Second)
	second := evaluateGate(t, g, d, resultOf(first))
	if n := len(f.receiver.received()); n != 2 {
		t.Fatalf("received %d calls, want a fresh request after a passed result", n)
	}
	if evidenceMap(t, second)["requests"] != float64(1) {
		t.Errorf("evidence = %s, want a fresh count", second.Evidence)
	}
	calls := f.receiver.received()
	if a, b := calls[0].header.Get(gates.HeaderIdempotencyKey), calls[1].header.Get(gates.HeaderIdempotencyKey); a == b {
		t.Errorf("idempotency key %q reused for a fresh request", a)
	}
}

func TestBackupGateUnreachable(t *testing.T) {
	t.Parallel()

	f := newBackupFixture(t)
	f.server.Close()
	g := f.gate(t)
	d := migrationDeployment(core.KindDeploy)

	ev := evaluateGate(t, g, d, nil)
	if ev.Status != core.GatePending || !strings.Contains(ev.Message, "post backup webhook") || !strings.HasSuffix(ev.Message, "; retrying") {
		t.Errorf("evaluation = %s %q, want pending with the transport error", ev.Status, ev.Message)
	}
	if strings.Contains(ev.Message, "hidden") || strings.Contains(ev.Message, "/hooks/backup") {
		t.Errorf("message leaks the webhook URL: %q", ev.Message)
	}
	evidence := evidenceMap(t, ev)
	if evidence["requests"] != float64(1) || evidence["status_code"] != float64(0) {
		t.Errorf("evidence = %v", evidence)
	}
	f.clock.Advance(time.Second)
	again := evaluateGate(t, g, d, resultOf(ev))
	if again.Message != ev.Message || string(again.Evidence) != string(ev.Evidence) {
		t.Errorf("retried within the poll interval: %q %s", again.Message, again.Evidence)
	}
}

func TestBackupGateClientTimeout(t *testing.T) {
	t.Parallel()

	f := newBackupFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.receiver.respond = func(w http.ResponseWriter, _ int) {
		<-release
		w.WriteHeader(http.StatusOK)
	}
	f.deps.Client.Timeout = 50 * time.Millisecond
	ev := evaluateGate(t, f.gate(t), migrationDeployment(core.KindDeploy), nil)
	if ev.Status != core.GatePending || !strings.Contains(ev.Message, "retrying") {
		t.Errorf("evaluation = %s %q, want pending", ev.Status, ev.Message)
	}
}

func TestBackupGateErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*config.Gate)
		secrets map[string]string
		want    string
	}{
		{name: "url not set", secrets: map[string]string{"BACKUP_SECRET": "x"}, want: "read backup webhook URL"},
		{name: "secret not set", secrets: map[string]string{"BACKUP_URL": "http://127.0.0.1:1/"}, want: "read backup webhook secret"},
		{name: "relative url", secrets: map[string]string{"BACKUP_URL": "/hooks", "BACKUP_SECRET": "x"}, want: "absolute http or https URL"},
		{name: "unsupported scheme", secrets: map[string]string{"BACKUP_URL": "ftp://backup.example.com/hooks", "BACKUP_SECRET": "x"}, want: "absolute http or https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newBackupFixture(t)
			f.secrets = tt.secrets
			_, err := f.gate(t).Evaluate(t.Context(), gates.Request{Deployment: migrationDeployment(core.KindDeploy), Phase: core.GatePhaseBefore})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Evaluate error = %v, want %q", err, tt.want)
			}
			if len(f.receiver.received()) != 0 {
				t.Error("posted despite the error")
			}
		})
	}
}

func TestBackupGateLiteralURL(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	g := f.gate(t, func(g *config.Gate) {
		g.Webhook = &config.GateWebhook{URL: f.server.URL + "/literal", SecretEnv: "BACKUP_SECRET"}
	})
	if ev := evaluateGate(t, g, migrationDeployment(core.KindDeploy), nil); ev.Status != core.GatePassed {
		t.Errorf("status = %s (%q), want passed", ev.Status, ev.Message)
	}
}

func TestBackupGateBoundsResponse(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	reference := strings.Repeat("r", 500)
	f.receiver.respond = answer(http.StatusOK, `{"status":"succeeded","reference":"`+reference+`"}`)
	ev := evaluateGate(t, f.gate(t), migrationDeployment(core.KindDeploy), nil)
	if got, _ := evidenceMap(t, ev)["reference"].(string); len(got) != 200 {
		t.Errorf("reference kept %d characters, want 200", len(got))
	}
	f2 := newBackupFixture(t)
	f2.receiver.respond = answer(http.StatusOK, strings.Repeat("x", 64<<10))
	ev = evaluateGate(t, f2.gate(t), migrationDeployment(core.KindDeploy), nil)
	if ev.Status != core.GatePassed || len(ev.Message) > core.MaxGateMessageBytes {
		t.Errorf("evaluation = %s with a %d byte message", ev.Status, len(ev.Message))
	}
}

func TestBackupGateCancelledContext(t *testing.T) {
	t.Parallel()
	f := newBackupFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.gate(t).Evaluate(ctx, gates.Request{Deployment: migrationDeployment(core.KindDeploy), Phase: core.GatePhaseBefore}); err == nil {
		t.Error("Evaluate succeeded with a cancelled context")
	}
}
