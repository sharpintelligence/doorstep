-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION notify_queue_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('doorstep_queue', TG_TABLE_SCHEMA);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER queue_entries_notify_insert AFTER INSERT ON queue_entries
    FOR EACH ROW EXECUTE FUNCTION notify_queue_change();

CREATE TRIGGER queue_entries_notify_delete AFTER DELETE ON queue_entries
    FOR EACH ROW EXECUTE FUNCTION notify_queue_change();

CREATE TRIGGER queue_entries_notify_update AFTER UPDATE ON queue_entries
    FOR EACH ROW
    WHEN (
        (OLD.lease_owner IS NOT NULL AND NEW.lease_owner IS NULL)
        OR OLD.priority IS DISTINCT FROM NEW.priority
        OR OLD.available_at IS DISTINCT FROM NEW.available_at
    )
    EXECUTE FUNCTION notify_queue_change();
