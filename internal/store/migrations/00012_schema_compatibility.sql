-- +goose Up
CREATE TABLE schema_compatibility (
    singleton boolean PRIMARY KEY DEFAULT true,
    min_binary_schema bigint NOT NULL,
    CONSTRAINT schema_compatibility_singleton_check CHECK (singleton),
    CONSTRAINT schema_compatibility_min_binary_schema_check CHECK (min_binary_schema > 0)
);

INSERT INTO schema_compatibility (min_binary_schema) VALUES (12);
