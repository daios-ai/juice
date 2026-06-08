PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE users (
    id              TEXT PRIMARY KEY,
    handle          TEXT NOT NULL UNIQUE,
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    available       INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked          INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    suspended_at    TEXT,
    public_key      TEXT,
    remote_base_url TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_users_public_key ON users(public_key) WHERE public_key IS NOT NULL;

CREATE TABLE actions (
    id               TEXT PRIMARY KEY,
    owner_user_id    TEXT NOT NULL REFERENCES users(id),
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('http','wasm','native','remote_proxy')),
    active           INTEGER NOT NULL DEFAULT 0,
    public           INTEGER NOT NULL DEFAULT 0,
    price            INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    description      TEXT NOT NULL DEFAULT '',
    input_schema     TEXT NOT NULL DEFAULT '{}',
    output_schema    TEXT NOT NULL DEFAULT '{}',
    source           TEXT NOT NULL DEFAULT '',
    artifact_hash    TEXT NOT NULL DEFAULT '',
    embed_vec        TEXT,
    remote_action_id TEXT NOT NULL DEFAULT '',
    deleted_at       TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE UNIQUE INDEX idx_actions_owner_name_active ON actions(owner_user_id, name) WHERE deleted_at IS NULL;
CREATE INDEX idx_actions_owner ON actions(owner_user_id);

CREATE TABLE processes (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES users(id),
    available     INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked        INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    created_at    TEXT NOT NULL,
    ended_at      TEXT
);

CREATE TABLE traces (
    id              TEXT PRIMARY KEY,
    process_id      TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id TEXT,
    action_owner_id TEXT NOT NULL DEFAULT '',
    cost            INTEGER NOT NULL DEFAULT 0,
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL
);

CREATE INDEX idx_traces_process ON traces(process_id);

CREATE TABLE transactions (
    id                  TEXT PRIMARY KEY,
    process_id          TEXT NOT NULL,
    trace_id            TEXT NOT NULL,
    parent_trace_id     TEXT NOT NULL,
    owner_user_id       TEXT NOT NULL,
    caller_user_id      TEXT NOT NULL,
    target_user_id      TEXT NOT NULL,
    action_id           TEXT NOT NULL,
    action_name         TEXT NOT NULL DEFAULT '',
    remote_action_id    TEXT NOT NULL DEFAULT '',
    args_json           TEXT NOT NULL DEFAULT '',
    reply_json          TEXT NOT NULL DEFAULT '',
    remote_receipt_json TEXT NOT NULL DEFAULT '',
    remote_receipt_hash TEXT,
    status              TEXT NOT NULL CHECK (status IN ('success','failure')),
    gross               INTEGER NOT NULL DEFAULT 0 CHECK (gross >= 0),
    net                 INTEGER NOT NULL DEFAULT 0 CHECK (net >= 0),
    fee                 INTEGER NOT NULL DEFAULT 0 CHECK (fee >= 0),
    reason              TEXT NOT NULL DEFAULT '',
    started_at          TEXT NOT NULL,
    ended_at            TEXT NOT NULL
);

CREATE INDEX idx_transactions_owner   ON transactions(owner_user_id);
CREATE INDEX idx_transactions_process ON transactions(process_id);
CREATE INDEX idx_transactions_trace   ON transactions(trace_id);

CREATE TABLE receipts (
    id             TEXT PRIMARY KEY,
    issuer_user_id TEXT NOT NULL REFERENCES users(id),
    tx_id          TEXT NOT NULL UNIQUE REFERENCES transactions(id),
    trace_id       TEXT NOT NULL,
    action_id      TEXT NOT NULL,
    caller_user_id TEXT NOT NULL DEFAULT '',
    process_id     TEXT NOT NULL DEFAULT '',
    started_at     TEXT NOT NULL DEFAULT '',
    args_hash      TEXT NOT NULL DEFAULT '',
    reply_hash     TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL,
    gross          INTEGER NOT NULL DEFAULT 0,
    net            INTEGER NOT NULL DEFAULT 0,
    fee            INTEGER NOT NULL DEFAULT 0,
    reason         TEXT NOT NULL DEFAULT '',
    signature      TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);

CREATE INDEX idx_receipts_tx             ON receipts(tx_id);
CREATE INDEX idx_receipts_action_started ON receipts(action_id, started_at DESC);

CREATE TABLE ratings (
    id               TEXT PRIMARY KEY,
    rated_tx_id      TEXT NOT NULL UNIQUE REFERENCES transactions(id),
    rated_receipt_id TEXT REFERENCES receipts(id),
    rater_user_id    TEXT NOT NULL REFERENCES users(id),
    rating           REAL NOT NULL CHECK (rating IN (0, 1)),
    note             TEXT,
    signature        TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
);

CREATE INDEX idx_ratings_tx ON ratings(rated_tx_id);

CREATE TABLE action_stats (
    action_id    TEXT PRIMARY KEY REFERENCES actions(id) ON DELETE CASCADE,
    uses         INTEGER NOT NULL DEFAULT 0,
    successes    INTEGER NOT NULL DEFAULT 0,
    failures     INTEGER NOT NULL DEFAULT 0,
    price_mean   REAL NOT NULL DEFAULT 0,
    latency_mean REAL NOT NULL DEFAULT 0,
    rating_mean  REAL NOT NULL DEFAULT 0,
    rating_count INTEGER NOT NULL DEFAULT 0,
    last_used_at TEXT NOT NULL
);

CREATE TABLE stat_tags (
    action_id  TEXT NOT NULL REFERENCES actions(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (action_id, key, source)
);

CREATE TABLE auth_codes (
    code           TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users(id),
    code_challenge TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    used           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE refresh_tokens (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id),
    expires_at TEXT NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE deposits (
    id               TEXT PRIMARY KEY,
    operator_user_id TEXT NOT NULL REFERENCES users(id),
    target_user_id   TEXT NOT NULL REFERENCES users(id),
    amount           INTEGER NOT NULL CHECK (amount > 0),
    reason           TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
);

CREATE TABLE idempotency_records (
    id                   TEXT PRIMARY KEY,
    idempotency_key      TEXT NOT NULL,
    counterparty_user_id TEXT NOT NULL REFERENCES users(id),
    receipt_id           TEXT REFERENCES receipts(id),
    status               TEXT NOT NULL DEFAULT 'complete',
    result_json          TEXT NOT NULL DEFAULT '',
    receipt_json         TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    expires_at           TEXT NOT NULL,
    UNIQUE (idempotency_key, counterparty_user_id)
);

CREATE INDEX idx_idempotency ON idempotency_records(idempotency_key, counterparty_user_id);

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

CREATE INDEX idx_steps_process_id     ON steps(process_id);
CREATE INDEX idx_steps_required_caller ON steps(required_caller_user_id);
CREATE INDEX idx_steps_status         ON steps(status);

CREATE TABLE config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
