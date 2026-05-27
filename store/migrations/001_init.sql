PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    handle        TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    available     INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked        INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS actions (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES users(id),
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('http','wasm','native')),
    active        INTEGER NOT NULL DEFAULT 0,
    price         INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    description   TEXT NOT NULL DEFAULT '',
    input_schema  TEXT NOT NULL DEFAULT '{}',
    output_schema TEXT NOT NULL DEFAULT '{}',
    source        TEXT NOT NULL DEFAULT '',
    artifact_hash TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (owner_user_id, name)
);

CREATE TABLE IF NOT EXISTS acl_entries (
    subject_user_id TEXT NOT NULL REFERENCES users(id),
    action_id       TEXT NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
    permission      TEXT NOT NULL CHECK (permission IN ('read','call','admin')),
    created_at      TEXT NOT NULL,
    PRIMARY KEY (subject_user_id, action_id, permission)
);

CREATE TABLE IF NOT EXISTS processes (
    id           TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES users(id),
    available    INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked       INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    status       TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    created_at   TEXT NOT NULL,
    ended_at     TEXT
);

CREATE TABLE IF NOT EXISTS traces (
    id              TEXT PRIMARY KEY,
    process_id      TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id TEXT NOT NULL,
    created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS transactions (
    id              TEXT PRIMARY KEY,
    process_id      TEXT NOT NULL,
    trace_id        TEXT NOT NULL,
    parent_trace_id TEXT NOT NULL,
    owner_user_id   TEXT NOT NULL,
    subject_user_id TEXT NOT NULL,
    target_user_id  TEXT NOT NULL,
    action_id       TEXT NOT NULL,
    args_json       TEXT NOT NULL DEFAULT '',
    reply_json      TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('success','failure')),
    gross           INTEGER NOT NULL DEFAULT 0 CHECK (gross >= 0),
    net             INTEGER NOT NULL DEFAULT 0 CHECK (net >= 0),
    fee             INTEGER NOT NULL DEFAULT 0 CHECK (fee >= 0),
    reason          TEXT NOT NULL DEFAULT '',
    started_at      TEXT NOT NULL,
    ended_at        TEXT NOT NULL,
    rating          REAL
);

CREATE TABLE IF NOT EXISTS action_stats (
    action_id    TEXT PRIMARY KEY REFERENCES actions(id) ON DELETE CASCADE,
    uses         INTEGER NOT NULL DEFAULT 0,
    successes    INTEGER NOT NULL DEFAULT 0,
    failures     INTEGER NOT NULL DEFAULT 0,
    price_mean   REAL NOT NULL DEFAULT 0,
    latency_mean REAL NOT NULL DEFAULT 0,
    rating_mean  REAL NOT NULL DEFAULT 0,
    last_used_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS stat_tags (
    action_id  TEXT NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (action_id, key, source)
);

CREATE TABLE IF NOT EXISTS listeners (
    id               TEXT PRIMARY KEY,
    owner_user_id    TEXT NOT NULL REFERENCES users(id),
    source_user_id   TEXT NOT NULL REFERENCES users(id),
    event_name       TEXT NOT NULL,
    process_id       TEXT NOT NULL REFERENCES processes(id),
    trace_id         TEXT NOT NULL,
    target_action_id TEXT NOT NULL REFERENCES actions(id),
    active           INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    listener_id TEXT NOT NULL REFERENCES listeners(id),
    tx_id       TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS auth_codes (
    code           TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id),
    code_challenge TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    used           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS refresh_tokens (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id),
    expires_at TEXT NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_actions_owner   ON actions(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_acl_action      ON acl_entries(action_id);
CREATE INDEX IF NOT EXISTS idx_transactions_owner  ON transactions(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_transactions_process ON transactions(process_id);
CREATE INDEX IF NOT EXISTS idx_transactions_trace ON transactions(trace_id);
CREATE INDEX IF NOT EXISTS idx_listeners_source_event ON listeners(source_user_id, event_name);
CREATE INDEX IF NOT EXISTS idx_events_listener ON events(listener_id);
CREATE INDEX IF NOT EXISTS idx_traces_process ON traces(process_id);
