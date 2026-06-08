DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS listeners;

CREATE TABLE steps (
    id                      TEXT PRIMARY KEY,
    process_id              TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id         TEXT,
    required_caller_user_id TEXT NOT NULL REFERENCES users(id),
    next_action_id          TEXT NOT NULL REFERENCES actions(id),
    partial_args            TEXT NOT NULL DEFAULT '{}',
    input_schema            TEXT NOT NULL DEFAULT '{}',
    status                  TEXT NOT NULL DEFAULT 'waiting'
                                CHECK(status IN ('waiting','running','done')),
    tx_id                   TEXT,
    created_at              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_steps_process_id ON steps(process_id);
CREATE INDEX idx_steps_required_caller ON steps(required_caller_user_id);
CREATE INDEX idx_steps_status ON steps(status);
