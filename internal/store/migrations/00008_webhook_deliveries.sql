-- +goose Up
CREATE TABLE webhook_deliveries (
    delivery_id text PRIMARY KEY
        CONSTRAINT webhook_deliveries_delivery_id_check CHECK (char_length(delivery_id) BETWEEN 1 AND 128),
    event text NOT NULL
        CONSTRAINT webhook_deliveries_event_check CHECK (event <> ''),
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webhook_deliveries_received_at_idx ON webhook_deliveries (received_at);

CREATE TABLE intake_poll_state (
    id boolean PRIMARY KEY DEFAULT true
        CONSTRAINT intake_poll_state_id_check CHECK (id),
    polled_at timestamptz NOT NULL DEFAULT '-infinity'
);

INSERT INTO intake_poll_state DEFAULT VALUES;

-- +goose Down
DROP TABLE intake_poll_state;

DROP TABLE webhook_deliveries;
