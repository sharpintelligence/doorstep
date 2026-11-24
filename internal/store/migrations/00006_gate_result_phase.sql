-- +goose Up
ALTER TABLE gate_results
    ADD COLUMN phase text NOT NULL DEFAULT 'before'
        CONSTRAINT gate_results_phase_check CHECK (phase IN ('before', 'after'));

ALTER TABLE gate_results ALTER COLUMN phase DROP DEFAULT;

ALTER TABLE gate_results
    ADD COLUMN type text NOT NULL DEFAULT 'unknown'
        CONSTRAINT gate_results_type_check CHECK (btrim(type) <> '');

ALTER TABLE gate_results ALTER COLUMN type DROP DEFAULT;
