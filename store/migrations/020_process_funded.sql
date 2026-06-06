-- Process-funded execution model: remove caused_by_trace_id, make parent_trace_id nullable
-- (NULL = root trace), add action_owner_id (the owner of the action currently executing).
CREATE TABLE traces_new (
    id              TEXT PRIMARY KEY,
    process_id      TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id TEXT,
    action_owner_id TEXT NOT NULL DEFAULT '',
    cost            INTEGER NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL
);
INSERT INTO traces_new (id, process_id, parent_trace_id, action_owner_id, cost, latency_ms, created_at)
SELECT id, process_id,
       CASE WHEN parent_trace_id = id THEN NULL ELSE parent_trace_id END,
       '', cost, latency_ms, created_at
FROM traces;
DROP TABLE traces;
ALTER TABLE traces_new RENAME TO traces;
CREATE INDEX IF NOT EXISTS idx_traces_process ON traces(process_id);
