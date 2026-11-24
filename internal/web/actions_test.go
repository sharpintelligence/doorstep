package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sharpintelligence/doorstep/internal/auth"
	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/deploys"
	"github.com/sharpintelligence/doorstep/internal/rollback"
	"github.com/sharpintelligence/doorstep/internal/store"
)

type actionCase struct {
	name         string
	path         string
	form         url.Values
	principal    *auth.Principal
	setup        func(f *fakes)
	wantStatus   int
	wantLocation string
	wantBody     string
	check        func(t *testing.T, f *fakes)
}

func runActions(t *testing.T, tests []actionCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := richFakes()
			if tt.setup != nil {
				tt.setup(f)
			}
			principal := tt.principal
			if principal == nil {
				principal = &deployer
			}
			var logs bytes.Buffer
			rec := serve(t, newMux(t, f.options(&logs)), call{method: http.MethodPost, path: tt.path, form: tt.form, principal: principal})
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d:\n%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Location"); got != tt.wantLocation {
				t.Errorf("Location = %q, want %q", got, tt.wantLocation)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body does not contain %q:\n%s", tt.wantBody, rec.Body.String())
			}
			if tt.wantStatus >= 400 && !strings.Contains(rec.Body.String(), `role="alert"`) && tt.wantStatus != http.StatusConflict {
				t.Errorf("error page without an alert")
			}
			if strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("internal error leaked into the page")
			}
			assertSecurityHeaders(t, rec)
			if tt.check != nil {
				tt.check(t, f)
			}
		})
	}
}

var inputPattern = regexp.MustCompile(`<input[^>]*\bname="([^"]+)"[^>]*\bvalue="([^"]*)"`)

var errBoom = errors.New("database password=hunter2 unreachable")

func TestDeployAction(t *testing.T) {
	t.Parallel()

	runActions(t, []actionCase{
		{
			name: "queued", path: "/apps/blog/deployments",
			form:       url.Values{"commit_sha": {" " + strings.ToUpper(shaApp) + " "}, "urgent": {"1"}, "reason": {"hotfix"}},
			wantStatus: http.StatusSeeOther, wantLocation: "/deployments/41?notice=deploy_queued",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				want := deploys.Request{Target: blog, CommitSHA: strings.ToUpper(shaApp), Urgent: true, Reason: "hotfix", Actor: alice, Role: core.RoleDeployer}
				if len(f.deploys.deploys) != 1 || f.deploys.deploys[0] != want {
					t.Errorf("Deploy requests = %+v, want %+v", f.deploys.deploys, want)
				}
			},
		},
		{
			name: "already queued", path: "/apps/blog/deployments", form: url.Values{"commit_sha": {shaApp}},
			setup:      func(f *fakes) { f.deploys.created = false },
			wantStatus: http.StatusSeeOther, wantLocation: "/deployments/41?notice=deploy_exists",
		},
		{
			name: "unknown app", path: "/apps/shop/deployments", form: url.Values{"commit_sha": {shaApp}},
			wantStatus: http.StatusNotFound, wantBody: "No app with this name",
		},
		{
			name: "invalid", path: "/apps/blog/deployments", form: url.Values{"commit_sha": {"abc"}},
			setup:      func(f *fakes) { f.deploys.deployErr = fmt.Errorf("%w: bad", deploys.ErrInvalidRequest) },
			wantStatus: http.StatusBadRequest, wantBody: `value="abc"`,
		},
		{
			name: "not found", path: "/apps/blog/deployments", form: url.Values{"commit_sha": {shaApp}},
			setup:      func(f *fakes) { f.deploys.deployErr = deploys.ErrCommitNotFound },
			wantStatus: http.StatusNotFound, wantBody: "does not exist in acme/blog",
		},
		{
			name: "not on branch", path: "/apps/blog/deployments", form: url.Values{"commit_sha": {shaApp}},
			setup:      func(f *fakes) { f.deploys.deployErr = deploys.ErrCommitNotOnBranch },
			wantStatus: http.StatusConflict, wantBody: "not on main of acme/blog",
		},
		{
			name: "blocked", path: "/apps/blog/deployments", form: url.Values{"commit_sha": {shaApp}},
			setup: func(f *fakes) {
				f.deploys.deployErr = &core.BlockedError{Block: core.Block{Target: blog, Commit: core.Commit{SHA: shaApp}, CreatedBy: "user:oidc:bob", Reason: "bad <b>"}}
			},
			wantStatus: http.StatusConflict, wantBody: "is blocked by user:oidc:bob: bad &lt;b&gt;",
		},
		{
			name: "unavailable", path: "/apps/blog/deployments", form: url.Values{"commit_sha": {shaApp}},
			setup:      func(f *fakes) { f.deploys.deployErr = fmt.Errorf("%w: %w", deploys.ErrUnavailable, errBoom) },
			wantStatus: http.StatusServiceUnavailable, wantBody: "GitHub or migration detection is unavailable",
		},
		{
			name: "internal", path: "/apps/blog/deployments", form: url.Values{"commit_sha": {shaApp}},
			setup:      func(f *fakes) { f.deploys.deployErr = errBoom },
			wantStatus: http.StatusInternalServerError, wantBody: "The error was logged",
		},
	})
}

func TestCancelAction(t *testing.T) {
	t.Parallel()

	runActions(t, []actionCase{
		{
			name: "cancelled", path: "/deployments/9/cancel", form: url.Values{"reason": {"wrong"}},
			wantStatus: http.StatusSeeOther, wantLocation: "/deployments/9?notice=deploy_cancelled",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				want := deploys.CancelRequest{ID: 9, Reason: "wrong", Actor: alice, Role: core.RoleDeployer}
				if len(f.deploys.cancels) != 1 || f.deploys.cancels[0] != want {
					t.Errorf("Cancel requests = %+v", f.deploys.cancels)
				}
			},
		},
		{
			name: "finished", path: "/deployments/9/cancel", form: url.Values{},
			setup:      func(f *fakes) { f.deploys.cancelErr = deploys.ErrNotCancellable },
			wantStatus: http.StatusConflict, wantBody: "already finished",
		},
		{
			name: "unknown", path: "/deployments/404/cancel", form: url.Values{},
			setup:      func(f *fakes) { f.deploys.cancelErr = core.ErrNotFound },
			wantStatus: http.StatusNotFound, wantBody: "Deployment 404 does not exist",
		},
		{
			name: "bad id", path: "/deployments/x/cancel", form: url.Values{},
			wantStatus: http.StatusNotFound, wantBody: "positive whole numbers",
		},
		{
			name: "reason too long", path: "/deployments/9/cancel", form: url.Values{"reason": {"long"}},
			setup:      func(f *fakes) { f.deploys.cancelErr = deploys.ErrInvalidRequest },
			wantStatus: http.StatusBadRequest, wantBody: `value="long"`,
		},
		{
			name: "contended", path: "/deployments/9/cancel", form: url.Values{},
			setup:      func(f *fakes) { f.deploys.cancelErr = core.ErrStateConflict },
			wantStatus: http.StatusServiceUnavailable, wantBody: "Try again",
		},
	})
}

func TestRollbackAction(t *testing.T) {
	t.Parallel()

	crossing := func(req rollback.Request) error {
		if req.ConfirmMigration {
			return nil
		}
		migration := deployment(9, core.StateSucceeded)
		migration.Migration, migration.MigrationReason = true, "label db-migration on #4"
		return fmt.Errorf("roll back: %w", &core.MigrationRollbackError{To: deployment(7, core.StateSucceeded), Migrations: []core.Deployment{migration}})
	}
	runActions(t, []actionCase{
		{
			name: "one click", path: "/apps/blog/rollback", form: url.Values{},
			wantStatus: http.StatusSeeOther, wantLocation: "/deployments/31?notice=rollback_queued",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				req := f.rollbacks.requests[0]
				if req.Target != blog || req.ToDeployment != nil || req.ConfirmMigration || req.Actor != alice || req.Role != core.RoleDeployer {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{
			name: "migration confirmation page", path: "/apps/blog/rollback", form: url.Values{"to_deployment_id": {"7"}, "reason": {"5xx <spike>"}},
			setup:      func(f *fakes) { f.rollbacks.rollbackErr = crossing },
			wantStatus: http.StatusConflict,
			wantBody:   `<input type="hidden" name="confirm_migration_rollback" value="1">`,
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				if len(f.rollbacks.requests) != 1 || *f.rollbacks.requests[0].ToDeployment != 7 {
					t.Errorf("requests = %+v", f.rollbacks.requests)
				}
			},
		},
		{
			name: "confirmed", path: "/apps/blog/rollback", form: url.Values{"to_deployment_id": {"7"}, "reason": {"5xx"}, "confirm_migration_rollback": {"1"}},
			setup:      func(f *fakes) { f.rollbacks.rollbackErr = crossing },
			wantStatus: http.StatusSeeOther, wantLocation: "/deployments/31?notice=rollback_queued",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				if req := f.rollbacks.requests[0]; !req.ConfirmMigration || req.Reason != "5xx" || *req.ToDeployment != 7 {
					t.Errorf("request = %+v", req)
				}
			},
		},
		{
			name: "bad target", path: "/apps/blog/rollback", form: url.Values{"to_deployment_id": {"seven"}},
			wantStatus: http.StatusBadRequest, wantBody: "must be a deployment id",
		},
		{
			name: "in progress", path: "/apps/blog/rollback", form: url.Values{},
			setup: func(f *fakes) {
				f.rollbacks.rollbackErr = func(rollback.Request) error {
					return &core.RollbackInProgressError{Deployment: deployment(30, core.StateQueued)}
				}
			},
			wantStatus: http.StatusConflict, wantBody: "rollback 30 of blog/default is already queued",
		},
		{
			name: "nothing to roll back to", path: "/apps/blog/rollback", form: url.Values{},
			setup: func(f *fakes) {
				f.rollbacks.rollbackErr = func(rollback.Request) error { return core.ErrNoRollbackTarget }
			},
			wantStatus: http.StatusConflict, wantBody: "no earlier successful",
		},
		{
			name: "target blocked", path: "/apps/blog/rollback", form: url.Values{},
			setup: func(f *fakes) {
				f.rollbacks.rollbackErr = func(rollback.Request) error {
					return &core.BlockedError{Block: core.Block{Target: blog, Digest: digestA, CreatedBy: "user:oidc:bob"}}
				}
			},
			wantStatus: http.StatusConflict, wantBody: "is blocked",
		},
		{
			name: "contended", path: "/apps/blog/rollback", form: url.Values{},
			setup: func(f *fakes) {
				f.rollbacks.rollbackErr = func(rollback.Request) error { return core.ErrStateConflict }
			},
			wantStatus: http.StatusServiceUnavailable, wantBody: "Try again",
		},
	})
}

func TestMigrationConfirmationRoundTrip(t *testing.T) {
	t.Parallel()

	f := richFakes()
	f.rollbacks.rollbackErr = func(req rollback.Request) error {
		if req.ConfirmMigration {
			return nil
		}
		return &core.MigrationRollbackError{To: deployment(7, core.StateSucceeded), Migrations: []core.Deployment{deployment(9, core.StateSucceeded)}}
	}
	mux := newMux(t, f.options(io.Discard))
	first := serve(t, mux, call{method: http.MethodPost, path: "/apps/blog/rollback", form: url.Values{"to_deployment_id": {"7"}, "reason": {"spike"}}, principal: &deployer})
	if first.Code != http.StatusConflict {
		t.Fatalf("first POST = %d", first.Code)
	}
	forms := formPattern.FindAllString(first.Body.String(), -1)
	var confirm string
	for _, form := range forms {
		if strings.Contains(form, "confirm_migration_rollback") {
			confirm = form
		}
	}
	if confirm == "" {
		t.Fatalf("no confirmation form:\n%s", first.Body.String())
	}
	fields := url.Values{}
	for _, m := range inputPattern.FindAllStringSubmatch(confirm, -1) {
		fields.Set(m[1], m[2])
	}
	if fields.Get("to_deployment_id") != "7" || fields.Get("reason") != "spike" || fields.Get("confirm_migration_rollback") != "1" {
		t.Fatalf("confirmation fields = %v", fields)
	}
	second := serve(t, mux, call{method: http.MethodPost, path: "/apps/blog/rollback", form: fields, principal: &deployer})
	if second.Code != http.StatusSeeOther || len(f.rollbacks.requests) != 2 || !f.rollbacks.requests[1].ConfirmMigration {
		t.Fatalf("second POST = %d, requests %+v", second.Code, f.rollbacks.requests)
	}
}

func TestBlockAction(t *testing.T) {
	t.Parallel()

	upperSHA := strings.ToUpper(shaApp)
	forwarded := func(wantRef core.BlockRef, wantReason string) func(t *testing.T, f *fakes) {
		return func(t *testing.T, f *fakes) {
			t.Helper()
			if len(f.rollbacks.blockReqs) != 1 {
				t.Fatalf("block requests = %+v, want one", f.rollbacks.blockReqs)
			}
			req := f.rollbacks.blockReqs[0]
			if req.Target != blog || req.Ref != wantRef || req.Reason != wantReason || req.Actor != alice || req.Role != core.RoleDeployer {
				t.Errorf("block request = %+v", req)
			}
		}
	}
	notForwarded := func(t *testing.T, f *fakes) {
		t.Helper()
		if len(f.rollbacks.blockReqs) != 0 {
			t.Errorf("block requests = %+v, want none", f.rollbacks.blockReqs)
		}
	}
	runActions(t, []actionCase{
		{
			name: "digest", path: "/apps/blog/block", form: url.Values{"ref": {" " + string(digestB) + " "}, "reason": {"leaks memory"}},
			wantStatus: http.StatusSeeOther, wantLocation: "/apps/blog?notice=blocked",
			check: forwarded(core.BlockRef{Digest: digestB}, "leaks memory"),
		},
		{
			name: "upper case commit", path: "/apps/blog/block", form: url.Values{"ref": {upperSHA}, "reason": {"bad"}},
			wantStatus: http.StatusSeeOther, wantLocation: "/apps/blog?notice=blocked",
			check: forwarded(core.BlockRef{CommitSHA: shaApp}, "bad"),
		},
		{
			name: "already blocked", path: "/apps/blog/block", form: url.Values{"ref": {shaApp}, "reason": {"again"}},
			setup:      func(f *fakes) { f.rollbacks.blockExists = true },
			wantStatus: http.StatusSeeOther, wantLocation: "/apps/blog?notice=block_exists",
			check: forwarded(core.BlockRef{CommitSHA: shaApp}, "again"),
		},
		{
			name: "malformed ref keeps the values", path: "/apps/blog/block", form: url.Values{"ref": {"latest"}, "reason": {"why"}},
			wantStatus: http.StatusBadRequest, wantBody: `value="latest"`,
			check: notForwarded,
		},
		{
			name: "malformed ref opens the disclosure", path: "/apps/blog/block", form: url.Values{"ref": {"latest"}, "reason": {"why"}},
			wantStatus: http.StatusBadRequest, wantBody: `<details class="card" open>` + "\n<summary>Block a version of blog",
			check: notForwarded,
		},
		{
			name: "missing reason", path: "/apps/blog/block", form: url.Values{"ref": {shaApp}},
			setup:      func(f *fakes) { f.rollbacks.blockErr = fmt.Errorf("%w: a reason is required", core.ErrInvalidBlock) },
			wantStatus: http.StatusBadRequest, wantBody: "a reason is required",
		},
		{name: "unknown app", path: "/apps/nope/block", form: url.Values{"ref": {shaApp}, "reason": {"x"}}, wantStatus: http.StatusNotFound, check: notForwarded},
		{
			name: "service error", path: "/apps/blog/block", form: url.Values{"ref": {shaApp}, "reason": {"x"}},
			setup:      func(f *fakes) { f.rollbacks.blockErr = errBoom },
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "viewer", path: "/apps/blog/block", form: url.Values{"ref": {shaApp}, "reason": {"x"}}, principal: &viewer,
			wantStatus: http.StatusForbidden, check: notForwarded,
		},
	})
}

func TestBlockAndLockActions(t *testing.T) {
	t.Parallel()

	runActions(t, []actionCase{
		{
			name: "unblock", path: "/apps/blog/unblock", form: url.Values{"ref": {string(digestB)}},
			wantStatus: http.StatusSeeOther, wantLocation: "/apps/blog?notice=unblocked",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				if len(f.rollbacks.unblocks) != 1 || f.rollbacks.unblocks[0].Digest != digestB {
					t.Errorf("unblocks = %+v", f.rollbacks.unblocks)
				}
			},
		},
		{name: "unblock bad ref", path: "/apps/blog/unblock", form: url.Values{"ref": {"latest"}}, wantStatus: http.StatusBadRequest, wantBody: "full commit SHA"},
		{
			name: "unblock nothing", path: "/apps/blog/unblock", form: url.Values{"ref": {shaApp}},
			setup:      func(f *fakes) { f.rollbacks.unblockErr = core.ErrNotFound },
			wantStatus: http.StatusNotFound, wantBody: "is not blocked for blog",
		},
		{
			name: "lock app", path: "/apps/blog/lock", form: url.Values{"reason": {"db work"}, "expires_in": {"2h"}},
			wantStatus: http.StatusSeeOther, wantLocation: "/apps/blog?notice=locked",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				req := f.locks.requests[0]
				if req.Target == nil || *req.Target != blog || req.Actor != alice || req.Role != core.RoleDeployer || req.Reason != "db work" || *req.ExpiresIn != 2*time.Hour {
					t.Errorf("lock request = %+v", req)
				}
			},
		},
		{
			name: "lock bad expiry", path: "/apps/blog/lock", form: url.Values{"reason": {"x"}, "expires_in": {"tomorrow"}},
			setup:      func(f *fakes) { f.locks.locks = nil },
			wantStatus: http.StatusBadRequest, wantBody: `value="tomorrow"`,
		},
		{
			name: "lock held", path: "/apps/blog/lock", form: url.Values{"reason": {"x"}},
			setup: func(f *fakes) {
				f.locks.lockErr = &core.LockHeldError{Lock: core.Lock{Target: blog, Owner: "user:oidc:bob", Reason: "mine"}}
			},
			wantStatus: http.StatusConflict, wantBody: "already locked by user:oidc:bob",
		},
		{
			name: "lock invalid", path: "/apps/blog/lock", form: url.Values{},
			setup:      func(f *fakes) { f.locks.lockErr = fmt.Errorf("%w: reason is required", core.ErrInvalidLock) },
			wantStatus: http.StatusBadRequest, wantBody: "reason is required",
		},
		{
			name: "lock contended", path: "/apps/blog/lock", form: url.Values{"reason": {"x"}},
			setup:      func(f *fakes) { f.locks.lockErr = core.ErrLockContended },
			wantStatus: http.StatusServiceUnavailable, wantBody: "Try again",
		},
		{
			name: "unlock app", path: "/apps/blog/unlock", form: url.Values{},
			wantStatus: http.StatusSeeOther, wantLocation: "/apps/blog?notice=unlocked",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				if len(f.locks.unlocks) != 1 || f.locks.unlocks[0] != "blog by user:oidc:alice" {
					t.Errorf("unlocks = %v", f.locks.unlocks)
				}
				if len(f.locks.roles) != 1 || f.locks.roles[0] != core.RoleDeployer {
					t.Errorf("unlock roles = %v, want deployer", f.locks.roles)
				}
			},
		},
		{
			name: "unlock unlocked app", path: "/apps/blog/unlock", form: url.Values{},
			setup:      func(f *fakes) { f.locks.unlockErr = core.ErrNotFound },
			wantStatus: http.StatusNotFound, wantBody: "blog is not locked",
		},
		{
			name: "global lock", path: "/locks/global", form: url.Values{"reason": {"release"}},
			wantStatus: http.StatusSeeOther, wantLocation: "/locks?notice=global_locked",
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				if req := f.locks.requests[0]; req.Target != nil || req.Reason != "release" {
					t.Errorf("lock request = %+v", req)
				}
			},
		},
		{
			name: "global lock held", path: "/locks/global", form: url.Values{"reason": {"release"}},
			setup: func(f *fakes) {
				f.locks.lockErr = &core.LockHeldError{Lock: core.Lock{Global: true, Owner: "user:oidc:bob"}}
			},
			wantStatus: http.StatusConflict, wantBody: "global is already locked",
		},
		{
			name: "global release", path: "/locks/global/release", form: url.Values{},
			wantStatus: http.StatusSeeOther, wantLocation: "/locks?notice=global_unlocked",
		},
		{
			name: "global release without lock", path: "/locks/global/release", form: url.Values{},
			setup:      func(f *fakes) { f.locks.unlockErr = core.ErrNotFound },
			wantStatus: http.StatusNotFound, wantBody: "no global lock",
		},
		{
			name: "form too large", path: "/apps/blog/lock", form: url.Values{"reason": {strings.Repeat("x", auth.MaxFormBytes)}},
			wantStatus: http.StatusRequestEntityTooLarge, wantBody: "larger than doorstep accepts",
		},
	})
}

func TestTokenActions(t *testing.T) {
	t.Parallel()

	runActions(t, []actionCase{
		{
			name: "created once", path: "/tokens", form: url.Values{"name": {"ci"}, "role": {"reporter"}, "expires_in": {"720h"}},
			principal:  &admin,
			wantStatus: http.StatusCreated, wantBody: `value="dsp_secretvalue"`,
			check: func(t *testing.T, f *fakes) {
				t.Helper()
				in := f.tokens.created[0]
				if in.Name != "ci" || in.Role != core.RoleReporter || in.ExpiresIn != 720*time.Hour || in.By.Actor != alice || in.By.Origin["via"] != "ui" {
					t.Errorf("create input = %+v", in)
				}
			},
		},
		{
			name: "duplicate", path: "/tokens", form: url.Values{"name": {"ci"}, "role": {"viewer"}},
			principal:  &admin,
			setup:      func(f *fakes) { f.tokens.createErr = store.ErrTokenExists },
			wantStatus: http.StatusConflict, wantBody: "already exists",
		},
		{
			name: "bad name", path: "/tokens", form: url.Values{"name": {"CI"}, "role": {"viewer"}},
			principal:  &admin,
			setup:      func(f *fakes) { f.tokens.createErr = fmt.Errorf("%w: bad", auth.ErrInvalidTokenName) },
			wantStatus: http.StatusBadRequest, wantBody: `value="CI"`,
		},
		{
			name: "bad expiry", path: "/tokens", form: url.Values{"name": {"ci"}, "role": {"viewer"}, "expires_in": {"-1h"}},
			principal:  &admin,
			wantStatus: http.StatusBadRequest, wantBody: "positive duration",
		},
		{
			name: "revoke", path: "/tokens/ci/revoke", form: url.Values{},
			principal:  &admin,
			wantStatus: http.StatusSeeOther, wantLocation: "/tokens?notice=token_revoked",
		},
		{
			name: "revoke unknown", path: "/tokens/ci/revoke", form: url.Values{},
			principal:  &admin,
			setup:      func(f *fakes) { f.tokens.revokeErr = core.ErrNotFound },
			wantStatus: http.StatusNotFound, wantBody: "no active token named ci",
		},
	})
}

func TestCreatedTokenIsNeverStored(t *testing.T) {
	t.Parallel()

	f := richFakes()
	rec := serve(t, newMux(t, f.options(io.Discard)), call{method: http.MethodPost, path: "/tokens", form: url.Values{"name": {"ci"}, "role": {"viewer"}}, principal: &admin})
	if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Location") != "" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("token response redirects or sets cookies: %v", rec.Header())
	}
	list := serve(t, newMux(t, f.options(io.Discard)), call{path: "/tokens", principal: &admin})
	if strings.Contains(list.Body.String(), "dsp_secretvalue") {
		t.Fatalf("token list shows the secret")
	}
}
