-- +goose Up
CREATE TABLE gate_reports (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id bigint NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    gate text NOT NULL
        CONSTRAINT gate_reports_gate_check CHECK (btrim(gate) <> ''),
    status text NOT NULL
        CONSTRAINT gate_reports_status_check CHECK (status IN ('pending', 'passed', 'failed')),
    message text NOT NULL DEFAULT '',
    url text NOT NULL DEFAULT '',
    details jsonb NOT NULL DEFAULT '{}'
        CONSTRAINT gate_reports_details_check CHECK (jsonb_typeof(details) = 'object'),
    reported_by text NOT NULL
        CONSTRAINT gate_reports_reported_by_check CHECK (btrim(reported_by) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gate_reports_deployment_id_gate_key UNIQUE (deployment_id, gate)
);

CREATE TRIGGER gate_reports_set_updated_at BEFORE UPDATE ON gate_reports
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_role_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_role_check
    CHECK (role IN ('viewer', 'reporter', 'deployer', 'admin'));

ALTER TABLE auth_sessions DROP CONSTRAINT auth_sessions_role_check;
ALTER TABLE auth_sessions ADD CONSTRAINT auth_sessions_role_check
    CHECK (role IN ('viewer', 'reporter', 'deployer', 'admin'));

ALTER TABLE audit_log DROP CONSTRAINT audit_log_actor_role_check;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_actor_role_check
    CHECK (actor_role IN ('viewer', 'reporter', 'deployer', 'admin'));

-- +goose Down
ALTER TABLE audit_log DROP CONSTRAINT audit_log_actor_role_check;
ALTER TABLE audit_log ADD CONSTRAINT audit_log_actor_role_check
    CHECK (actor_role IN ('viewer', 'deployer', 'admin')) NOT VALID;

DELETE FROM auth_sessions WHERE role = 'reporter';
ALTER TABLE auth_sessions DROP CONSTRAINT auth_sessions_role_check;
ALTER TABLE auth_sessions ADD CONSTRAINT auth_sessions_role_check
    CHECK (role IN ('viewer', 'deployer', 'admin'));

DELETE FROM api_tokens WHERE role = 'reporter';
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_role_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_role_check
    CHECK (role IN ('viewer', 'deployer', 'admin'));

DROP TABLE gate_reports;
