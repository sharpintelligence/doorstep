-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TABLE apps (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL CONSTRAINT apps_name_check CHECK (btrim(name) <> ''),
    environment text NOT NULL CONSTRAINT apps_environment_check CHECK (btrim(environment) <> ''),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT apps_name_environment_key UNIQUE (name, environment)
);

CREATE TABLE deployments (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_id bigint NOT NULL REFERENCES apps (id),
    kind text NOT NULL
        CONSTRAINT deployments_kind_check CHECK (kind IN ('deploy', 'rollback')),
    state text NOT NULL
        CONSTRAINT deployments_state_check CHECK (state IN (
            'queued', 'resolving', 'pinning', 'syncing', 'verifying',
            'succeeded', 'failed', 'cancelled', 'superseded'
        )),
    source text NOT NULL
        CONSTRAINT deployments_source_check CHECK (source IN ('webhook', 'poll', 'api', 'cli', 'system')),
    requested_by text NOT NULL DEFAULT '',
    source_repository text NOT NULL
        CONSTRAINT deployments_source_repository_check CHECK (btrim(source_repository) <> ''),
    commit_sha text NOT NULL
        CONSTRAINT deployments_commit_sha_check CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    image_repository text NOT NULL
        CONSTRAINT deployments_image_repository_check CHECK (btrim(image_repository) <> ''),
    image_tag text NOT NULL
        CONSTRAINT deployments_image_tag_check CHECK (btrim(image_tag) <> ''),
    image_digest text
        CONSTRAINT deployments_image_digest_check CHECK (image_digest ~ '^sha256:[0-9a-f]{64}$'),
    previous_digest text
        CONSTRAINT deployments_previous_digest_check CHECK (previous_digest ~ '^sha256:[0-9a-f]{64}$'),
    pin_commit_sha text
        CONSTRAINT deployments_pin_commit_sha_check CHECK (pin_commit_sha ~ '^[0-9a-f]{40}$'),
    parent_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    rollback_of_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    superseded_by_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    failure_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    CONSTRAINT deployments_id_app_id_key UNIQUE (id, app_id)
);

CREATE INDEX deployments_app_id_idx ON deployments (app_id, id DESC);

CREATE INDEX deployments_parent_id_idx ON deployments (parent_id) WHERE parent_id IS NOT NULL;

CREATE INDEX deployments_rollback_of_id_idx ON deployments (rollback_of_id) WHERE rollback_of_id IS NOT NULL;

CREATE INDEX deployments_superseded_by_id_idx ON deployments (superseded_by_id) WHERE superseded_by_id IS NOT NULL;

CREATE UNIQUE INDEX deployments_one_active_per_app ON deployments (app_id)
    WHERE state IN ('resolving', 'pinning', 'syncing', 'verifying');

CREATE TRIGGER deployments_set_updated_at BEFORE UPDATE ON deployments
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE queue_entries (
    deployment_id bigint PRIMARY KEY,
    app_id bigint NOT NULL REFERENCES apps (id),
    priority integer NOT NULL DEFAULT 0,
    enqueued_at timestamptz NOT NULL DEFAULT now(),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_expires_at timestamptz,
    attempts integer NOT NULL DEFAULT 0
        CONSTRAINT queue_entries_attempts_check CHECK (attempts >= 0),
    CONSTRAINT queue_entries_lease_check CHECK (
        (lease_owner IS NULL AND lease_expires_at IS NULL)
        OR (lease_owner IS NOT NULL AND btrim(lease_owner) <> '' AND lease_expires_at IS NOT NULL)
    ),
    CONSTRAINT queue_entries_deployment_fkey FOREIGN KEY (deployment_id, app_id)
        REFERENCES deployments (id, app_id) ON DELETE CASCADE
);

CREATE INDEX queue_entries_app_order_idx ON queue_entries (app_id, priority DESC, deployment_id);

CREATE INDEX queue_entries_available_at_idx ON queue_entries (available_at);

CREATE TABLE locks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_id bigint NOT NULL REFERENCES apps (id),
    kind text NOT NULL
        CONSTRAINT locks_kind_check CHECK (kind IN ('deployment', 'freeze')),
    owner text NOT NULL
        CONSTRAINT locks_owner_check CHECK (btrim(owner) <> ''),
    reason text NOT NULL DEFAULT '',
    deployment_id bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    CONSTRAINT locks_app_id_kind_key UNIQUE (app_id, kind),
    CONSTRAINT locks_deployment_fkey FOREIGN KEY (deployment_id, app_id)
        REFERENCES deployments (id, app_id) ON DELETE CASCADE
);

CREATE INDEX locks_deployment_id_idx ON locks (deployment_id) WHERE deployment_id IS NOT NULL;

CREATE TABLE gate_results (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id bigint NOT NULL REFERENCES deployments (id) ON DELETE CASCADE,
    name text NOT NULL
        CONSTRAINT gate_results_name_check CHECK (btrim(name) <> ''),
    status text NOT NULL
        CONSTRAINT gate_results_status_check CHECK (status IN ('pending', 'passed', 'failed', 'skipped')),
    message text NOT NULL DEFAULT '',
    evidence jsonb NOT NULL DEFAULT '{}',
    started_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gate_results_deployment_id_name_key UNIQUE (deployment_id, name)
);

CREATE TRIGGER gate_results_set_updated_at BEFORE UPDATE ON gate_results
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE deployment_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    deployment_id bigint NOT NULL,
    app_id bigint NOT NULL REFERENCES apps (id),
    type text NOT NULL
        CONSTRAINT deployment_events_type_check CHECK (type IN ('transition', 'gate', 'lock', 'note')),
    from_state text
        CONSTRAINT deployment_events_from_state_check CHECK (from_state IN (
            'queued', 'resolving', 'pinning', 'syncing', 'verifying',
            'succeeded', 'failed', 'cancelled', 'superseded'
        )),
    to_state text
        CONSTRAINT deployment_events_to_state_check CHECK (to_state IN (
            'queued', 'resolving', 'pinning', 'syncing', 'verifying',
            'succeeded', 'failed', 'cancelled', 'superseded'
        )),
    actor text NOT NULL DEFAULT '',
    message text NOT NULL DEFAULT '',
    data jsonb NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT deployment_events_deployment_fkey FOREIGN KEY (deployment_id, app_id)
        REFERENCES deployments (id, app_id) ON DELETE CASCADE
);

CREATE INDEX deployment_events_deployment_id_idx ON deployment_events (deployment_id, id);

CREATE INDEX deployment_events_app_id_idx ON deployment_events (app_id, id DESC);
