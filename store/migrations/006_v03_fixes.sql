-- Rebuild traces with CHECK constraints and new columns needed for crash recovery
-- and federation retry (action_id, caller_user_id, idempotency_key, dispatch_json).
CREATE TABLE traces_new (
    id               TEXT PRIMARY KEY,
    process_id       TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id  TEXT REFERENCES traces_new(id),
    action_owner_id  TEXT NOT NULL DEFAULT '',
    action_id        TEXT NOT NULL DEFAULT '',
    caller_user_id   TEXT NOT NULL DEFAULT '',
    available        INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked           INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    latency_ms       INTEGER NOT NULL DEFAULT 0,
    idempotency_key  TEXT,
    dispatch_json    TEXT,
    created_at       TEXT NOT NULL
);
INSERT INTO traces_new (id,process_id,parent_trace_id,action_owner_id,available,locked,latency_ms,created_at)
    SELECT id,process_id,parent_trace_id,action_owner_id,available,locked,latency_ms,created_at FROM traces;
DROP TABLE traces;
ALTER TABLE traces_new RENAME TO traces;
CREATE INDEX idx_traces_process_id ON traces(process_id);

-- completion_trace_id: set when a step-completion call is in-flight, cleared on re-park.
-- Enables crash recovery to safely re-park interrupted step completions.
ALTER TABLE steps ADD COLUMN completion_trace_id TEXT;

-- auth_json: AES-256-GCM encrypted upstream API credentials (write-only; never read onto Action struct).
ALTER TABLE actions ADD COLUMN auth_json TEXT NOT NULL DEFAULT '';
