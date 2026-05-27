-- Replace the old push-style events table with a persistent pending/in-flight/consumed queue.
DROP INDEX IF EXISTS idx_events_listener;
DROP TABLE IF EXISTS events;

CREATE TABLE events (
    id               TEXT PRIMARY KEY,
    listener_id      TEXT NOT NULL REFERENCES listeners(id) ON DELETE CASCADE,
    args_json        TEXT NOT NULL DEFAULT '{}',
    causing_trace_id TEXT,
    consumed_at      TEXT,
    tx_id            TEXT,
    created_at       TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_events_listener_pending
    ON events(listener_id, created_at) WHERE consumed_at IS NULL;
