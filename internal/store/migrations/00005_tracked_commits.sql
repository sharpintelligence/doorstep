-- +goose Up
CREATE TABLE tracked_commits (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_id bigint NOT NULL REFERENCES apps (id),
    repository text NOT NULL
        CONSTRAINT tracked_commits_repository_check CHECK (btrim(repository) <> ''),
    branch text NOT NULL
        CONSTRAINT tracked_commits_branch_check CHECK (btrim(branch) <> ''),
    commit_sha text NOT NULL
        CONSTRAINT tracked_commits_commit_sha_check CHECK (commit_sha ~ '^[0-9a-f]{40}$'),
    source text NOT NULL
        CONSTRAINT tracked_commits_source_check CHECK (source IN ('webhook', 'poll', 'api', 'cli', 'system')),
    pushed_by text NOT NULL DEFAULT '',
    checks_state text NOT NULL DEFAULT 'pending'
        CONSTRAINT tracked_commits_checks_state_check CHECK (checks_state IN ('pending', 'success', 'failure')),
    checks jsonb NOT NULL DEFAULT '[]'
        CONSTRAINT tracked_commits_checks_check CHECK (jsonb_typeof(checks) = 'array'),
    deployment_id bigint REFERENCES deployments (id) ON DELETE SET NULL,
    superseded_by_id bigint REFERENCES tracked_commits (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    evaluated_at timestamptz,
    enqueued_at timestamptz,
    CONSTRAINT tracked_commits_app_id_commit_sha_key UNIQUE (app_id, commit_sha),
    CONSTRAINT tracked_commits_enqueued_check CHECK (deployment_id IS NULL OR enqueued_at IS NOT NULL),
    CONSTRAINT tracked_commits_deployment_id_key UNIQUE (deployment_id)
);

CREATE INDEX tracked_commits_commit_sha_idx ON tracked_commits (commit_sha);

CREATE INDEX tracked_commits_open_idx ON tracked_commits (app_id)
    WHERE enqueued_at IS NULL AND superseded_by_id IS NULL;

CREATE INDEX tracked_commits_superseded_by_id_idx ON tracked_commits (superseded_by_id)
    WHERE superseded_by_id IS NOT NULL;

CREATE TRIGGER tracked_commits_set_updated_at BEFORE UPDATE ON tracked_commits
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
