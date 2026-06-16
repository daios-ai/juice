-- v0.4: Remove cached latency from traces; simplify steps (drop process_id and input_schema;
-- rename next_action_id → action_id).

-- Rebuild traces without latency_ms.
CREATE TABLE traces_new (
    id               TEXT PRIMARY KEY,
    process_id       TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id  TEXT REFERENCES traces_new(id),
    action_owner_id  TEXT NOT NULL DEFAULT '',
    action_id        TEXT NOT NULL DEFAULT '',
    caller_user_id   TEXT NOT NULL DEFAULT '',
    available        INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked           INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    idempotency_key  TEXT,
    dispatch_json    TEXT,
    created_at       TEXT NOT NULL
);
INSERT INTO traces_new (id,process_id,parent_trace_id,action_owner_id,action_id,caller_user_id,available,locked,idempotency_key,dispatch_json,created_at)
    SELECT id,process_id,parent_trace_id,action_owner_id,action_id,caller_user_id,available,locked,idempotency_key,dispatch_json,created_at FROM traces;
DROP TABLE traces;
ALTER TABLE traces_new RENAME TO traces;
CREATE INDEX idx_traces_process_id ON traces(process_id);

-- Rebuild steps: drop process_id and input_schema; rename next_action_id → action_id.
CREATE TABLE steps_new (
    id                      TEXT PRIMARY KEY,
    parent_trace_id         TEXT REFERENCES traces(id),
    required_caller_user_id TEXT NOT NULL REFERENCES users(id),
    action_id               TEXT NOT NULL REFERENCES actions(id),
    partial_args            TEXT NOT NULL DEFAULT '{}',
    price                   INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    status                  TEXT NOT NULL DEFAULT 'waiting'
                                CHECK(status IN ('waiting','running','done','cancelled')),
    tx_id                   TEXT,
    completion_trace_id     TEXT REFERENCES traces(id),
    created_at              TEXT NOT NULL
);
INSERT INTO steps_new (id,parent_trace_id,required_caller_user_id,action_id,partial_args,price,status,tx_id,completion_trace_id,created_at)
    SELECT id,parent_trace_id,required_caller_user_id,next_action_id,partial_args,price,status,tx_id,completion_trace_id,created_at FROM steps;
DROP TABLE steps;
ALTER TABLE steps_new RENAME TO steps;
CREATE INDEX idx_steps_required_caller ON steps(required_caller_user_id);
CREATE INDEX idx_steps_status          ON steps(status);
