-- +goose Up
ALTER TABLE deployments
    ADD COLUMN migration boolean NOT NULL DEFAULT false,
    ADD COLUMN migration_reason text NOT NULL DEFAULT '',
    ADD CONSTRAINT deployments_migration_reason_check
        CHECK (char_length(migration_reason) <= 200 AND (migration OR migration_reason = ''));

CREATE INDEX deployments_app_migration_idx ON deployments (app_id, id)
    WHERE migration AND kind = 'deploy';

-- +goose Down
DROP INDEX deployments_app_migration_idx;

ALTER TABLE deployments
    DROP CONSTRAINT deployments_migration_reason_check,
    DROP COLUMN migration_reason,
    DROP COLUMN migration;
