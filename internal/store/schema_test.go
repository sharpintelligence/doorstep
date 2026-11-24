package store_test

import (
	"strings"
	"testing"

	"github.com/sharpintelligence/doorstep/internal/core"
	"github.com/sharpintelligence/doorstep/internal/store"
	"github.com/sharpintelligence/doorstep/internal/store/storetest"
)

const insertRawDeployment = `
INSERT INTO deployments (app_id, kind, state, source, source_repository, commit_sha, image_repository, image_tag, image_digest)
VALUES ($1, $2, $3, $4, 'acme/web', $5, 'ghcr.io/acme/web', 'v1', $6)
RETURNING id`

type rawDeployment struct {
	kind   string
	state  string
	source string
	sha    string
	digest *string
}

func validRaw() rawDeployment {
	return rawDeployment{kind: "deploy", state: "queued", source: "api", sha: testSHA}
}

func insertRaw(t *testing.T, st *store.Store, app string, d rawDeployment) (int64, error) {
	t.Helper()
	appID, err := st.EnsureApp(t.Context(), core.Target{App: app, Environment: "prod"})
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	var id int64
	err = store.QueryRowSQL(t.Context(), st, insertRawDeployment, appID, d.kind, d.state, d.source, d.sha, d.digest).Scan(&id)
	return id, err
}

func TestSchemaAcceptsEveryCoreValue(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for _, s := range core.States() {
		d := validRaw()
		d.state = string(s)
		if _, err := insertRaw(t, st, "state-"+string(s), d); err != nil {
			t.Errorf("state %s rejected: %v", s, err)
		}
	}
	for _, k := range core.Kinds() {
		d := validRaw()
		d.kind = string(k)
		if _, err := insertRaw(t, st, "kind-"+string(k), d); err != nil {
			t.Errorf("kind %s rejected: %v", k, err)
		}
	}
	for _, s := range core.Sources() {
		d := validRaw()
		d.source = string(s)
		if _, err := insertRaw(t, st, "source-"+string(s), d); err != nil {
			t.Errorf("source %s rejected: %v", s, err)
		}
	}

	deploymentID, err := insertRaw(t, st, "children", validRaw())
	if err != nil {
		t.Fatalf("insert deployment: %v", err)
	}
	appID, err := st.EnsureApp(t.Context(), core.Target{App: "children", Environment: "prod"})
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	for _, s := range core.GateStatuses() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO gate_results (deployment_id, name, type, phase, status, evidence) VALUES ($1, $2, 'checks', 'before', $3, '{\"ok\":true}')", deploymentID, "gate-"+string(s), s); err != nil {
			t.Errorf("gate status %s rejected: %v", s, err)
		}
	}
	for _, p := range core.GatePhases() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO gate_results (deployment_id, name, type, phase, status) VALUES ($1, $2, 'health', $3, 'pending')", deploymentID, "phase-"+string(p), p); err != nil {
			t.Errorf("gate phase %s rejected: %v", p, err)
		}
	}
	for _, k := range core.LockKinds() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO locks (app_id, kind, owner, deployment_id) VALUES ($1, $2, 'ops', $3)", appID, k, deploymentID); err != nil {
			t.Errorf("lock kind %s rejected: %v", k, err)
		}
	}
	for _, a := range core.LockActions() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO lock_events (app_id, action, owner) VALUES ($1, $2, 'ops')", appID, a); err != nil {
			t.Errorf("lock action %s rejected: %v", a, err)
		}
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO lock_events (action, owner) VALUES ($1, 'ops')", a); err != nil {
			t.Errorf("global lock action %s rejected: %v", a, err)
		}
	}
	if err := store.ExecSQL(t.Context(), st, "INSERT INTO locks (kind, owner) VALUES ('freeze', 'ops')"); err != nil {
		t.Errorf("global freeze rejected: %v", err)
	}
	for _, e := range core.EventTypes() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO deployment_events (deployment_id, app_id, type) VALUES ($1, $2, $3)", deploymentID, appID, e); err != nil {
			t.Errorf("event type %s rejected: %v", e, err)
		}
	}
	for _, s := range core.States() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO deployment_events (deployment_id, app_id, type, from_state, to_state) VALUES ($1, $2, 'transition', $3, $3)", deploymentID, appID, s); err != nil {
			t.Errorf("event state %s rejected: %v", s, err)
		}
	}
	for _, r := range core.Roles() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO api_tokens (name, token_hash, role, created_by) VALUES ($1, sha256($3::bytea), $2, 'cli:root')", "token-"+string(r), r, "token-"+string(r)); err != nil {
			t.Errorf("token role %s rejected: %v", r, err)
		}
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO auth_sessions (token_hash, provider, subject, username, role, expires_at) VALUES (sha256($1::bytea), 'github', '1', 'octocat', $2, now())", "session-"+string(r), r); err != nil {
			t.Errorf("session role %s rejected: %v", r, err)
		}
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO audit_log (actor_kind, actor_id, actor_role, action) VALUES ('user', 'octocat', $1, 'auth.login')", r); err != nil {
			t.Errorf("audit role %s rejected: %v", r, err)
		}
	}
	for _, k := range core.ActorKinds() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO audit_log (actor_kind, actor_id, action) VALUES ($1, 'x', 'auth.login')", k); err != nil {
			t.Errorf("actor kind %s rejected: %v", k, err)
		}
	}
	for _, a := range core.AuditActions() {
		if err := store.ExecSQL(t.Context(), st, "INSERT INTO audit_log (actor_kind, action) VALUES ('system', $1)", a); err != nil {
			t.Errorf("audit action %s rejected: %v", a, err)
		}
	}
}

func TestSchemaRejectsInvalidAuthRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sql        string
		constraint string
	}{
		{name: "unknown token role", sql: "INSERT INTO api_tokens (name, token_hash, role, created_by) VALUES ('ci', sha256('x'), 'root', 'cli:root')", constraint: "api_tokens_role_check"},
		{name: "short token hash", sql: "INSERT INTO api_tokens (name, token_hash, role, created_by) VALUES ('ci', 'abc'::bytea, 'viewer', 'cli:root')", constraint: "api_tokens_token_hash_check"},
		{name: "uppercase token name", sql: "INSERT INTO api_tokens (name, token_hash, role, created_by) VALUES ('CI', sha256('x'), 'viewer', 'cli:root')", constraint: "api_tokens_name_check"},
		{name: "token name too long", sql: "INSERT INTO api_tokens (name, token_hash, role, created_by) VALUES (repeat('a', 64), sha256('x'), 'viewer', 'cli:root')", constraint: "api_tokens_name_check"},
		{name: "blank token creator", sql: "INSERT INTO api_tokens (name, token_hash, role, created_by) VALUES ('ci', sha256('x'), 'viewer', ' ')", constraint: "api_tokens_created_by_check"},
		{name: "unknown session role", sql: "INSERT INTO auth_sessions (token_hash, provider, subject, username, role, expires_at) VALUES (sha256('x'), 'oidc', 's', 'u', 'root', now())", constraint: "auth_sessions_role_check"},
		{name: "long session hash", sql: "INSERT INTO auth_sessions (token_hash, provider, subject, username, role, expires_at) VALUES (sha512('x'), 'oidc', 's', 'u', 'viewer', now())", constraint: "auth_sessions_token_hash_check"},
		{name: "short login state", sql: "INSERT INTO auth_logins (state_hash, nonce, pkce_verifier, return_to, expires_at) VALUES ('x'::bytea, 'n', 'v', '/', now())", constraint: "auth_logins_state_hash_check"},
		{name: "unknown actor kind", sql: "INSERT INTO audit_log (actor_kind, action) VALUES ('robot', 'auth.login')", constraint: "audit_log_actor_kind_check"},
		{name: "unknown actor role", sql: "INSERT INTO audit_log (actor_kind, actor_role, action) VALUES ('system', 'root', 'auth.login')", constraint: "audit_log_actor_role_check"},
		{name: "action without dot", sql: "INSERT INTO audit_log (actor_kind, action) VALUES ('system', 'login')", constraint: "audit_log_action_check"},
		{name: "uppercase action", sql: "INSERT INTO audit_log (actor_kind, action) VALUES ('system', 'Auth.login')", constraint: "audit_log_action_check"},
		{name: "action with digit", sql: "INSERT INTO audit_log (actor_kind, action) VALUES ('system', 'auth.login2')", constraint: "audit_log_action_check"},
		{name: "app without environment", sql: "INSERT INTO audit_log (actor_kind, action, app) VALUES ('system', 'lock.acquire', 'web')", constraint: "audit_log_target_check"},
		{name: "detail array", sql: "INSERT INTO audit_log (actor_kind, action, detail) VALUES ('system', 'lock.acquire', '[]')", constraint: "audit_log_detail_check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			assertConstraint(t, store.ExecSQL(t.Context(), st, tt.sql), tt.constraint)
		})
	}
}

func TestSchemaRejectsInvalidIntakeRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sql        string
		constraint string
	}{
		{name: "empty delivery id", sql: "INSERT INTO webhook_deliveries (delivery_id, event) VALUES ('', 'push')", constraint: "webhook_deliveries_delivery_id_check"},
		{name: "delivery id too long", sql: "INSERT INTO webhook_deliveries (delivery_id, event) VALUES (repeat('x', 129), 'push')", constraint: "webhook_deliveries_delivery_id_check"},
		{name: "empty event", sql: "INSERT INTO webhook_deliveries (delivery_id, event) VALUES ('d', '')", constraint: "webhook_deliveries_event_check"},
		{name: "second poll state row", sql: "INSERT INTO intake_poll_state (id) VALUES (false)", constraint: "intake_poll_state_id_check"},
		{name: "duplicate poll state row", sql: "INSERT INTO intake_poll_state DEFAULT VALUES", constraint: "intake_poll_state_pkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			assertConstraint(t, store.ExecSQL(t.Context(), st, tt.sql), tt.constraint)
		})
	}
}

func TestSchemaRejectsInvalidDeployments(t *testing.T) {
	t.Parallel()

	short := "sha256:abc"
	upper := "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"
	tag := "latest"
	tests := []struct {
		name       string
		mutate     func(*rawDeployment)
		constraint string
	}{
		{name: "unknown state", mutate: func(d *rawDeployment) { d.state = "paused" }, constraint: "deployments_state_check"},
		{name: "unknown kind", mutate: func(d *rawDeployment) { d.kind = "hotfix" }, constraint: "deployments_kind_check"},
		{name: "unknown source", mutate: func(d *rawDeployment) { d.source = "cron" }, constraint: "deployments_source_check"},
		{name: "short SHA", mutate: func(d *rawDeployment) { d.sha = testSHA[:7] }, constraint: "deployments_commit_sha_check"},
		{name: "uppercase SHA", mutate: func(d *rawDeployment) { d.sha = "0123456789ABCDEF0123456789abcdef01234567" }, constraint: "deployments_commit_sha_check"},
		{name: "SHA with trailing newline", mutate: func(d *rawDeployment) { d.sha = testSHA + "\n" }, constraint: "deployments_commit_sha_check"},
		{name: "short digest", mutate: func(d *rawDeployment) { d.digest = &short }, constraint: "deployments_image_digest_check"},
		{name: "uppercase digest", mutate: func(d *rawDeployment) { d.digest = &upper }, constraint: "deployments_image_digest_check"},
		{name: "tag as digest", mutate: func(d *rawDeployment) { d.digest = &tag }, constraint: "deployments_image_digest_check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := validRaw()
			tt.mutate(&d)
			_, err := insertRaw(t, st, "web", d)
			assertConstraint(t, err, tt.constraint)
		})
	}
}

func TestSchemaRejectsInvalidRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sql        string
		constraint string
	}{
		{name: "empty app name", sql: "INSERT INTO apps (name, environment) VALUES (' ', 'prod')", constraint: "apps_name_check"},
		{name: "duplicate app", sql: "INSERT INTO apps (name, environment) VALUES ('web', 'prod')", constraint: "apps_name_environment_key"},
		{name: "unknown gate status", sql: "INSERT INTO gate_results (deployment_id, name, type, phase, status) VALUES ($1, 'soak', 'soak', 'after', 'running')", constraint: "gate_results_status_check"},
		{name: "unknown gate phase", sql: "INSERT INTO gate_results (deployment_id, name, type, phase, status) VALUES ($1, 'soak', 'soak', 'during', 'pending')", constraint: "gate_results_phase_check"},
		{name: "blank gate type", sql: "INSERT INTO gate_results (deployment_id, name, type, phase, status) VALUES ($1, 'soak', ' ', 'after', 'pending')", constraint: "gate_results_type_check"},
		{name: "unknown lock kind", sql: "INSERT INTO locks (app_id, kind, owner) VALUES ((SELECT app_id FROM deployments WHERE id = $1), 'manual', 'ops')", constraint: "locks_kind_check"},
		{name: "unknown event type", sql: "INSERT INTO deployment_events (deployment_id, app_id, type) VALUES ($1, $2, 'comment')", constraint: "deployment_events_type_check"},
		{name: "unknown event state", sql: "INSERT INTO deployment_events (deployment_id, app_id, type, to_state) VALUES ($1, $2, 'transition', 'paused')", constraint: "deployment_events_to_state_check"},
		{name: "lease owner without expiry", sql: "UPDATE queue_entries SET lease_owner = 'w1' WHERE deployment_id = $1", constraint: "queue_entries_lease_check"},
		{name: "lease expiry without owner", sql: "UPDATE queue_entries SET lease_expires_at = now() WHERE deployment_id = $1", constraint: "queue_entries_lease_check"},
		{name: "blank lease owner", sql: "UPDATE queue_entries SET lease_owner = '', lease_expires_at = now() WHERE deployment_id = $1", constraint: "queue_entries_lease_check"},
		{name: "second queue entry", sql: "INSERT INTO queue_entries (deployment_id, app_id) VALUES ($1, $2)", constraint: "queue_entries_pkey"},
		{name: "queue entry for another app", sql: withOtherApp + "UPDATE queue_entries q SET app_id = other.id FROM other WHERE q.deployment_id = $1", constraint: "queue_entries_deployment_fkey"},
		{name: "event for another app", sql: withOtherApp + "INSERT INTO deployment_events (deployment_id, app_id, type) SELECT $1, id, 'note' FROM other", constraint: "deployment_events_deployment_fkey"},
		{name: "lock for another app", sql: withOtherApp + "INSERT INTO locks (app_id, kind, owner, deployment_id) SELECT id, 'deployment', 'w1', $1 FROM other", constraint: "locks_deployment_fkey"},
		{name: "global deployment lock", sql: "INSERT INTO locks (kind, owner) VALUES ('deployment', 'ops')", constraint: "locks_global_check"},
		{name: "global freeze naming a deployment", sql: "INSERT INTO locks (kind, owner, deployment_id) VALUES ('freeze', 'ops', $1)", constraint: "locks_global_check"},
		{name: "second global freeze", sql: "WITH first AS (INSERT INTO locks (kind, owner) VALUES ('freeze', 'a') RETURNING id) INSERT INTO locks (kind, owner) SELECT 'freeze', 'b' FROM first", constraint: "locks_global_kind_key"},
		{name: "lock expiring at creation", sql: "INSERT INTO locks (app_id, kind, owner, expires_at) VALUES ((SELECT app_id FROM deployments WHERE id = $1), 'freeze', 'ops', now())", constraint: "locks_expires_at_check"},
		{name: "lock expired before creation", sql: "INSERT INTO locks (kind, owner, expires_at) VALUES ('freeze', 'ops', now() - interval '1 minute')", constraint: "locks_expires_at_check"},
		{name: "unknown lock action", sql: "INSERT INTO lock_events (action, owner) VALUES ('stolen', 'ops')", constraint: "lock_events_action_check"},
		{name: "lock event for unknown app", sql: "INSERT INTO lock_events (app_id, action, owner) VALUES (-1, 'locked', 'ops')", constraint: "lock_events_app_id_fkey"},
		{name: "blank gitops repository", sql: "UPDATE deployments SET gitops_repository = ' ' WHERE id = $1", constraint: "deployments_gitops_repository_check"},
		{name: "empty argo cd revision", sql: "UPDATE deployments SET argocd_revision = '' WHERE id = $1", constraint: "deployments_argocd_revision_check"},
		{name: "deployment moved to another app", sql: withOtherApp + "UPDATE deployments d SET app_id = other.id FROM other WHERE d.id = $1", constraint: "queue_entries_deployment_fkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			appID, err := st.EnsureApp(t.Context(), d.Target)
			if err != nil {
				t.Fatalf("EnsureApp: %v", err)
			}
			var args []any
			if strings.Contains(tt.sql, "$1") {
				args = append(args, d.ID)
			}
			if strings.Contains(tt.sql, "$2") {
				args = append(args, appID)
			}
			err = store.ExecSQL(t.Context(), st, tt.sql, args...)
			assertConstraint(t, err, tt.constraint)
		})
	}
}

const withOtherApp = "WITH other AS (INSERT INTO apps (name, environment) VALUES ('other', 'prod') RETURNING id) "

func TestSchemaIndexesEveryForeignKey(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	rows, err := store.QueryRowsSQL(t.Context(), st, `
		SELECT c.conrelid::regclass::text || '.' || c.conname
		FROM pg_constraint c
		WHERE c.contype = 'f'
		  AND c.connamespace = current_schema()::regnamespace
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_index i
		      WHERE i.indrelid = c.conrelid AND i.indkey[0] = c.conkey[1]
		  )
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("query foreign keys: %v", err)
	}
	for _, fk := range rows {
		t.Errorf("foreign key %s has no index leading with its first column", fk)
	}
}

func TestSchemaMaintainsUpdatedAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		setup  string
		update string
		read   string
	}{
		{
			name:   "deployments",
			update: "UPDATE deployments SET failure_reason = 'boom', updated_at = '2000-01-01' WHERE id = $1",
			read:   "SELECT updated_at > created_at AND updated_at > now() - interval '1 minute' FROM deployments WHERE id = $1",
		},
		{
			name:   "tracked_commits",
			setup:  "INSERT INTO tracked_commits (app_id, repository, branch, commit_sha, source) SELECT app_id, 'acme/web', 'main', commit_sha, 'webhook' FROM deployments WHERE id = $1",
			update: "UPDATE tracked_commits SET checks_state = 'success', updated_at = '2000-01-01' WHERE app_id = (SELECT app_id FROM deployments WHERE id = $1)",
			read:   "SELECT updated_at > created_at AND updated_at > now() - interval '1 minute' FROM tracked_commits WHERE app_id = (SELECT app_id FROM deployments WHERE id = $1)",
		},
		{
			name:   "gate_results",
			setup:  "INSERT INTO gate_results (deployment_id, name, type, phase, status) VALUES ($1, 'health', 'health', 'after', 'pending')",
			update: "UPDATE gate_results SET status = 'passed', updated_at = '2000-01-01' WHERE deployment_id = $1",
			read:   "SELECT updated_at > started_at AND updated_at > now() - interval '1 minute' FROM gate_results WHERE deployment_id = $1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t)
			d := mustCreate(t, st, newDeployment("web"), 0)
			if tt.setup != "" {
				mustExec(t, st, tt.setup, d.ID)
			}
			mustExec(t, st, tt.update, d.ID)
			var refreshed bool
			if err := store.QueryRowSQL(t.Context(), st, tt.read, d.ID).Scan(&refreshed); err != nil {
				t.Fatalf("read updated_at: %v", err)
			}
			if !refreshed {
				t.Errorf("%s.updated_at was not set to the update time", tt.name)
			}
		})
	}
}

func TestSchemaRejectsDuplicateLocksAndGates(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	d := mustCreate(t, st, newDeployment("web"), 0)
	appID, err := st.EnsureApp(t.Context(), d.Target)
	if err != nil {
		t.Fatalf("EnsureApp: %v", err)
	}
	mustExec(t, st, "INSERT INTO locks (app_id, kind, owner) VALUES ($1, 'freeze', 'ops')", appID)
	err = store.ExecSQL(t.Context(), st, "INSERT INTO locks (app_id, kind, owner) VALUES ($1, 'freeze', 'someone-else')", appID)
	assertConstraint(t, err, "locks_app_id_kind_key")

	mustExec(t, st, "INSERT INTO gate_results (deployment_id, name, type, phase, status) VALUES ($1, 'health', 'health', 'after', 'pending')", d.ID)
	err = store.ExecSQL(t.Context(), st, "INSERT INTO gate_results (deployment_id, name, type, phase, status) VALUES ($1, 'health', 'health', 'after', 'passed')", d.ID)
	assertConstraint(t, err, "gate_results_deployment_id_name_key")
}

func TestSchemaOneActiveDeploymentPerApp(t *testing.T) {
	t.Parallel()

	for _, first := range core.ActiveStates() {
		for _, second := range core.ActiveStates() {
			t.Run(string(first)+"+"+string(second), func(t *testing.T) {
				t.Parallel()
				st := storetest.Open(t)
				a := validRaw()
				a.state = string(first)
				if _, err := insertRaw(t, st, "web", a); err != nil {
					t.Fatalf("insert first: %v", err)
				}
				b := validRaw()
				b.state = string(second)
				_, err := insertRaw(t, st, "web", b)
				assertConstraint(t, err, "deployments_one_active_per_app")

				if _, err := insertRaw(t, st, "api", b); err != nil {
					t.Errorf("active deployment for another app rejected: %v", err)
				}
			})
		}
	}
}

func TestSchemaAllowsManyInactiveDeploymentsPerApp(t *testing.T) {
	t.Parallel()

	st := storetest.Open(t)
	for _, s := range core.States() {
		if s.Active() {
			continue
		}
		for range 2 {
			d := validRaw()
			d.state = string(s)
			if _, err := insertRaw(t, st, "web", d); err != nil {
				t.Errorf("inactive state %s rejected: %v", s, err)
			}
		}
	}
	d := validRaw()
	d.state = string(core.StateVerifying)
	if _, err := insertRaw(t, st, "web", d); err != nil {
		t.Errorf("one active deployment alongside inactive ones rejected: %v", err)
	}
}
