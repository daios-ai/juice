ALTER TABLE users ADD COLUMN denied_at TEXT;

CREATE TABLE traces_new (
    id              TEXT PRIMARY KEY,
    process_id      TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id TEXT REFERENCES traces_new(id),
    action_owner_id TEXT NOT NULL DEFAULT '',
    available       INTEGER NOT NULL DEFAULT 0,
    locked          INTEGER NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL
);
INSERT INTO traces_new (id,process_id,parent_trace_id,action_owner_id,available,locked,latency_ms,created_at)
    SELECT id,process_id,parent_trace_id,action_owner_id,0,0,latency_ms,created_at FROM traces;
DROP TABLE traces;
ALTER TABLE traces_new RENAME TO traces;
CREATE INDEX idx_traces_process_id ON traces(process_id);

ALTER TABLE steps ADD COLUMN price INTEGER NOT NULL DEFAULT 0;

CREATE TABLE action_stats_new (
    action_id        TEXT PRIMARY KEY REFERENCES actions(id),
    uses             INTEGER NOT NULL DEFAULT 0,
    successes        INTEGER NOT NULL DEFAULT 0,
    failures         INTEGER NOT NULL DEFAULT 0,
    rating_count     INTEGER NOT NULL DEFAULT 0,
    latency_estimate REAL    NOT NULL DEFAULT 0,
    rating_estimate  REAL    NOT NULL DEFAULT 0,
    last_used_at     TEXT    NOT NULL
);
INSERT INTO action_stats_new (action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at)
    SELECT action_id,uses,successes,failures,rating_count,latency_estimate,rating_estimate,last_used_at FROM action_stats;
DROP TABLE action_stats;
ALTER TABLE action_stats_new RENAME TO action_stats;

CREATE TABLE withdrawals (
    id                TEXT PRIMARY KEY,
    operator_user_id  TEXT NOT NULL REFERENCES users(id),
    target_user_id    TEXT NOT NULL REFERENCES users(id),
    amount            INTEGER NOT NULL,
    reason            TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL
);

CREATE TABLE discovered_kernels (
    public_key    TEXT NOT NULL,
    introduced_by TEXT NOT NULL,
    handle        TEXT NOT NULL DEFAULT '',
    base_url      TEXT NOT NULL DEFAULT '',
    stats_json    TEXT NOT NULL DEFAULT '{}',
    first_seen    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (public_key, introduced_by)
);
