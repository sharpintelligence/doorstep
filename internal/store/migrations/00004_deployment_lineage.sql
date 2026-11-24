-- +goose Up
ALTER TABLE deployments
    ADD COLUMN gitops_repository text
        CONSTRAINT deployments_gitops_repository_check CHECK (btrim(gitops_repository) <> ''),
    ADD COLUMN argocd_revision text
        CONSTRAINT deployments_argocd_revision_check CHECK (btrim(argocd_revision) <> '');

CREATE INDEX deployments_last_succeeded_idx ON deployments (app_id, finished_at DESC, id DESC)
    WHERE state = 'succeeded';
