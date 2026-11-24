-- +goose Up
CREATE TABLE api_tokens (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL
        CONSTRAINT api_tokens_name_check CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,62}$'),
    token_hash bytea NOT NULL
        CONSTRAINT api_tokens_token_hash_check CHECK (octet_length(token_hash) = 32),
    role text NOT NULL
        CONSTRAINT api_tokens_role_check CHECK (role IN ('viewer', 'deployer', 'admin')),
    created_by text NOT NULL
        CONSTRAINT api_tokens_created_by_check CHECK (btrim(created_by) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT api_tokens_name_key UNIQUE (name),
    CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash)
);

CREATE TABLE auth_sessions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_hash bytea NOT NULL
        CONSTRAINT auth_sessions_token_hash_check CHECK (octet_length(token_hash) = 32),
    provider text NOT NULL
        CONSTRAINT auth_sessions_provider_check CHECK (provider IN ('oidc', 'github')),
    subject text NOT NULL
        CONSTRAINT auth_sessions_subject_check CHECK (btrim(subject) <> ''),
    username text NOT NULL
        CONSTRAINT auth_sessions_username_check CHECK (btrim(username) <> ''),
    name text NOT NULL DEFAULT '',
    email text NOT NULL DEFAULT '',
    role text NOT NULL
        CONSTRAINT auth_sessions_role_check CHECK (role IN ('viewer', 'deployer', 'admin')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CONSTRAINT auth_sessions_token_hash_key UNIQUE (token_hash)
);

CREATE INDEX auth_sessions_expires_at_idx ON auth_sessions (expires_at);

CREATE TABLE auth_logins (
    state_hash bytea PRIMARY KEY
        CONSTRAINT auth_logins_state_hash_check CHECK (octet_length(state_hash) = 32),
    nonce text NOT NULL
        CONSTRAINT auth_logins_nonce_check CHECK (nonce <> ''),
    pkce_verifier text NOT NULL
        CONSTRAINT auth_logins_pkce_verifier_check CHECK (pkce_verifier <> ''),
    return_to text NOT NULL
        CONSTRAINT auth_logins_return_to_check CHECK (return_to LIKE '/%'),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

CREATE INDEX auth_logins_expires_at_idx ON auth_logins (expires_at);

CREATE TABLE audit_log (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor_kind text NOT NULL
        CONSTRAINT audit_log_actor_kind_check CHECK (actor_kind IN ('user', 'token', 'cli', 'system')),
    actor_id text NOT NULL DEFAULT '',
    actor_name text NOT NULL DEFAULT '',
    actor_role text
        CONSTRAINT audit_log_actor_role_check CHECK (actor_role IN ('viewer', 'deployer', 'admin')),
    action text NOT NULL
        CONSTRAINT audit_log_action_check CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
    app text,
    environment text,
    deployment_id bigint,
    detail jsonb NOT NULL DEFAULT '{}'
        CONSTRAINT audit_log_detail_check CHECK (jsonb_typeof(detail) = 'object'),
    CONSTRAINT audit_log_target_check CHECK ((app IS NULL) = (environment IS NULL))
);

CREATE INDEX audit_log_occurred_at_idx ON audit_log (occurred_at DESC, id DESC);

CREATE INDEX audit_log_target_idx ON audit_log (app, environment, id DESC) WHERE app IS NOT NULL;

CREATE INDEX audit_log_actor_idx ON audit_log (actor_kind, actor_id, id DESC);

CREATE INDEX audit_log_deployment_id_idx ON audit_log (deployment_id) WHERE deployment_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION audit_log_reject_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only'
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_reject_change();

CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_reject_change();
