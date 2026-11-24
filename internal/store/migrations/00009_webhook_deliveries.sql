-- +goose Up
CREATE TABLE webhook_deliveries (
    delivery_id text PRIMARY KEY
        CONSTRAINT webhook_deliveries_delivery_id_check CHECK (char_length(delivery_id) BETWEEN 1 AND 128),
    event text NOT NULL
        CONSTRAINT webhook_deliveries_event_check CHECK (event <> ''),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CONSTRAINT webhook_deliveries_completed_at_check CHECK (completed_at IS NULL OR completed_at >= claimed_at)
);

CREATE INDEX webhook_deliveries_claimed_at_idx ON webhook_deliveries (claimed_at);

CREATE TABLE intake_poll_state (
    id boolean PRIMARY KEY DEFAULT true
        CONSTRAINT intake_poll_state_id_check CHECK (id),
    next_poll_at timestamptz NOT NULL DEFAULT '-infinity'
);

INSERT INTO intake_poll_state DEFAULT VALUES;

-- +goose Down
DROP TABLE intake_poll_state;

DROP TABLE webhook_deliveries;
