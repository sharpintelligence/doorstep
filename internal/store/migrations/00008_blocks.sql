-- +goose Up
CREATE TABLE blocks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_id bigint NOT NULL REFERENCES apps (id),
    digest text
        CONSTRAINT blocks_digest_check CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    commit_sha text
        CONSTRAINT blocks_commit_sha_check CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    source_repository text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    created_by text NOT NULL
        CONSTRAINT blocks_created_by_check CHECK (btrim(created_by) <> ''),
    deployment_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    rollback_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    block_commit_sha text
        CONSTRAINT blocks_block_commit_sha_check CHECK (block_commit_sha ~ '^[0-9a-f]{40}$'),
    cleared_at timestamptz,
    cleared_by text
        CONSTRAINT blocks_cleared_by_check CHECK (btrim(cleared_by) <> ''),
    unblock_commit_sha text
        CONSTRAINT blocks_unblock_commit_sha_check CHECK (unblock_commit_sha ~ '^[0-9a-f]{40}$'),
    CONSTRAINT blocks_subject_check CHECK (digest IS NOT NULL OR commit_sha IS NOT NULL),
    CONSTRAINT blocks_cleared_check CHECK ((cleared_at IS NULL) = (cleared_by IS NULL)),
    CONSTRAINT blocks_unblock_check CHECK (unblock_commit_sha IS NULL OR cleared_at IS NOT NULL)
);

CREATE UNIQUE INDEX blocks_active_digest_key ON blocks (app_id, digest)
    WHERE cleared_at IS NULL AND digest IS NOT NULL;

CREATE UNIQUE INDEX blocks_active_commit_key ON blocks (app_id, commit_sha)
    WHERE cleared_at IS NULL AND digest IS NULL;

CREATE INDEX blocks_active_commit_idx ON blocks (app_id, commit_sha)
    WHERE cleared_at IS NULL AND commit_sha IS NOT NULL;

CREATE INDEX blocks_app_id_idx ON blocks (app_id, id);

CREATE INDEX blocks_deployment_id_idx ON blocks (deployment_id);

CREATE INDEX blocks_rollback_id_idx ON blocks (rollback_id);
