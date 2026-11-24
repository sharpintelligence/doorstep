-- +goose Up
ALTER TABLE locks ALTER COLUMN app_id DROP NOT NULL;

ALTER TABLE locks ADD COLUMN reminded_at timestamptz;

ALTER TABLE locks ADD CONSTRAINT locks_global_check
    CHECK (app_id IS NOT NULL OR (kind = 'freeze' AND deployment_id IS NULL));

ALTER TABLE locks ADD CONSTRAINT locks_expires_at_check
    CHECK (expires_at IS NULL OR expires_at > created_at);

CREATE UNIQUE INDEX locks_global_kind_key ON locks (kind) WHERE app_id IS NULL;

CREATE TABLE lock_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_id bigint REFERENCES apps (id),
    action text NOT NULL
        CONSTRAINT lock_events_action_check CHECK (action IN ('locked', 'unlocked', 'expired', 'reminded')),
    owner text NOT NULL,
    actor text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    expires_at timestamptz,
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX lock_events_app_id_idx ON lock_events (app_id, id DESC);
