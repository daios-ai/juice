-- Baseline schema. This is the whole schema: the tables, indexes, and triggers a Juice kernel
-- runs on. Earlier incremental migrations were folded into it once every live database had
-- reached that state, so a fresh database creates this directly and an existing one is
-- normalized and stamped by the runner (store/sqlite.go reconcileBaseline).
--
-- Identifiers are quoted exactly as a live database holds them, so a fresh schema and an
-- upgraded one are byte-identical in sqlite_master, not merely equivalent.
--
-- Future schema changes continue the sequence at 043_*.sql; this file is never edited in place
-- once released, because an existing database has already recorded it as applied.

CREATE TABLE "accounts" (
    id                  TEXT PRIMARY KEY,
    handle              TEXT,
    description         TEXT NOT NULL DEFAULT '',
    password_hash       TEXT NOT NULL DEFAULT '',
    recovery_public_key TEXT,
    available           INTEGER NOT NULL DEFAULT 0,
    locked              INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    suspended_at        TEXT,
    kernel_public_key   TEXT REFERENCES kernels(public_key),
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    CHECK (kernel_public_key IS NOT NULL OR available >= 0),
    CHECK (kernel_public_key IS NULL
           OR (handle IS NULL AND password_hash = '' AND recovery_public_key IS NULL))
);

CREATE TABLE "action_stats" (
    action_id        TEXT PRIMARY KEY REFERENCES actions(id),
    uses             INTEGER NOT NULL DEFAULT 0,
    successes        INTEGER NOT NULL DEFAULT 0,
    failures         INTEGER NOT NULL DEFAULT 0,
    rating_count     INTEGER NOT NULL DEFAULT 0,
    latency_estimate REAL    NOT NULL DEFAULT 0,
    rating_estimate  REAL    NOT NULL DEFAULT 0,
    last_used_at     TEXT    NOT NULL
);

CREATE TABLE actions (
    id               TEXT PRIMARY KEY,
    owner_user_id    TEXT NOT NULL REFERENCES "accounts"(id),
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('http','wasm','native','remote_proxy')),
    active           INTEGER NOT NULL DEFAULT 0,
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
, wasm_artifact TEXT NOT NULL DEFAULT '', auth_json TEXT NOT NULL DEFAULT '', visibility TEXT NOT NULL DEFAULT 'private'
    CHECK (visibility IN ('private','local','public')), remote_owner_id TEXT, remote_bps INTEGER, effect TEXT, base_price INTEGER);

CREATE VIRTUAL TABLE actions_fts USING fts5(action_id UNINDEXED, text);

CREATE TABLE auth_codes (
    code           TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES "accounts"(id),
    code_challenge TEXT NOT NULL,
    redirect_uri   TEXT NOT NULL DEFAULT '',
    expires_at     TEXT NOT NULL,
    used           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE connections (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES "accounts"(id),
    provider_key  TEXT NOT NULL,
    sealed_secret TEXT NOT NULL,
    scopes_json   TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (user_id, provider_key)
);

CREATE TABLE discovery_docs (
    kernel_public_key TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('user','action')),
    user_id           TEXT NOT NULL DEFAULT '',
    handle            TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    action_id         TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    input_schema      TEXT NOT NULL DEFAULT '{}',
    output_schema     TEXT NOT NULL DEFAULT '{}',
    embed_vec         TEXT,
    observed_at       TEXT NOT NULL, serving_price INTEGER NOT NULL DEFAULT 0
    CHECK (serving_price >= 0), effect TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (kernel_public_key, kind, user_id, action_id)
);

CREATE VIRTUAL TABLE discovery_fts USING fts5(doc_key UNINDEXED, text);

CREATE TABLE evidence (
    issuer_public_key             TEXT NOT NULL,
    receipt_hash                  TEXT NOT NULL,
    subject_kernel_public_key     TEXT NOT NULL,
    subject_action_id             TEXT NOT NULL,
    counterparty_kernel_public_key TEXT NOT NULL DEFAULT '',
    evidence_receipt_json         TEXT NOT NULL,
    rating_json                   TEXT NOT NULL DEFAULT '',
    remote_receipt_hash           TEXT NOT NULL DEFAULT '',
    receipt_created_at            TEXT NOT NULL,
    effective_at                  TEXT NOT NULL,
    observed_at                   TEXT NOT NULL,
    equivocated                   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (issuer_public_key, receipt_hash)
);

CREATE TABLE "grants" (
    id              TEXT PRIMARY KEY,
    grantor_user_id TEXT NOT NULL REFERENCES "accounts"(id),
    action_id       TEXT NOT NULL REFERENCES actions(id),
    connection_id   TEXT REFERENCES connections(id),
    created_at      TEXT NOT NULL,
    UNIQUE (grantor_user_id, action_id)
);

CREATE TABLE idempotency_records (
    id                   TEXT PRIMARY KEY,
    idempotency_key      TEXT NOT NULL,
    counterparty_user_id TEXT NOT NULL REFERENCES "accounts"(id),
    receipt_id           TEXT REFERENCES receipts(id),
    status               TEXT NOT NULL DEFAULT 'complete',
    result_json          TEXT NOT NULL DEFAULT '',
    receipt_json         TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    expires_at           TEXT NOT NULL,
    UNIQUE (idempotency_key, counterparty_user_id)
);

CREATE TABLE kernels (
    public_key    TEXT PRIMARY KEY,
    petname       TEXT,
    nickname      TEXT NOT NULL DEFAULT '',
    about         TEXT NOT NULL DEFAULT '',
    gossip_cursor TEXT NOT NULL DEFAULT '',
    last_seen     TEXT,
    peer_credit   INTEGER,
    first_seen    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE ledger (
    id               TEXT PRIMARY KEY,
    operator_user_id TEXT NOT NULL REFERENCES "accounts"(id),
    from_user_id     TEXT REFERENCES "accounts"(id),
    to_user_id       TEXT REFERENCES "accounts"(id),
    amount           INTEGER NOT NULL CHECK (amount > 0),
    reason           TEXT NOT NULL DEFAULT '',
    external_key     TEXT,
    created_at       TEXT NOT NULL,
    CHECK (from_user_id IS NOT NULL OR to_user_id IS NOT NULL)
);

CREATE TABLE pending_transfers (
    id              TEXT PRIMARY KEY,
    buyer_id        TEXT NOT NULL REFERENCES "accounts"(id),
    peer_key        TEXT NOT NULL,
    step_id         TEXT NOT NULL,
    input_hash      TEXT NOT NULL,
    input           TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    beneficiary     TEXT NOT NULL,
    amount          INTEGER NOT NULL,
    remote_max      INTEGER NOT NULL,
    reserve         INTEGER NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    last_error      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE processes (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES "accounts"(id),
    available     INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked        INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','closed')),
    created_at    TEXT NOT NULL,
    ended_at      TEXT
);

CREATE TABLE ratings (
    id               TEXT PRIMARY KEY,
    rated_tx_id      TEXT NOT NULL UNIQUE REFERENCES transactions(id),
    rated_receipt_id TEXT REFERENCES receipts(id),
    rater_user_id    TEXT NOT NULL REFERENCES "accounts"(id),
    rating           REAL NOT NULL CHECK (rating IN (0, 1)),
    note             TEXT,
    signature        TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
, rated_receipt_hash TEXT NOT NULL DEFAULT '');

CREATE TABLE receipts (
    id             TEXT PRIMARY KEY,
    issuer_user_id TEXT NOT NULL REFERENCES "accounts"(id),
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
, charge INTEGER NOT NULL DEFAULT 0, premium INTEGER NOT NULL DEFAULT 0, value INTEGER NOT NULL DEFAULT 0, value_premium INTEGER NOT NULL DEFAULT 0, value_to TEXT);

CREATE TABLE recovery_challenge (
    nonce      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE TABLE refresh_tokens (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES "accounts"(id),
    expires_at TEXT NOT NULL,
    revoked    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);

CREATE TABLE "steps" (
    id                      TEXT PRIMARY KEY,
    parent_trace_id         TEXT REFERENCES traces(id),
    required_caller_user_id TEXT NOT NULL REFERENCES "accounts"(id),
    action_id               TEXT NOT NULL REFERENCES actions(id),
    partial_args            TEXT NOT NULL DEFAULT '{}',
    price                   INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    status                  TEXT NOT NULL DEFAULT 'waiting'
                                CHECK(status IN ('waiting','running','done','cancelled')),
    tx_id                   TEXT,
    completion_trace_id     TEXT REFERENCES traces(id),
    created_at              TEXT NOT NULL
, required_caller_remote_id TEXT, import_bps INTEGER);

CREATE TABLE "traces" (
    id               TEXT PRIMARY KEY,
    process_id       TEXT NOT NULL REFERENCES processes(id),
    parent_trace_id  TEXT REFERENCES "traces"(id),
    action_owner_id  TEXT NOT NULL DEFAULT '',
    action_id        TEXT NOT NULL DEFAULT '',
    caller_user_id   TEXT NOT NULL DEFAULT '',
    available        INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked           INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    idempotency_key  TEXT,
    dispatch_json    TEXT,
    created_at       TEXT NOT NULL
, idempotency_record_id TEXT, premium_bps INTEGER, premium_parked INTEGER, value INTEGER, value_to TEXT, value_reserve INTEGER);

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
, refund INTEGER NOT NULL DEFAULT 0 CHECK (refund >= 0));

CREATE UNIQUE INDEX idx_accounts_handle ON accounts(handle) WHERE handle IS NOT NULL;

CREATE UNIQUE INDEX idx_accounts_kernel ON accounts(kernel_public_key) WHERE kernel_public_key IS NOT NULL;

CREATE INDEX idx_actions_owner ON actions(owner_user_id);

CREATE UNIQUE INDEX idx_actions_owner_name_active ON actions(owner_user_id, name) WHERE deleted_at IS NULL;

CREATE INDEX idx_evidence_cap ON evidence(issuer_public_key, subject_kernel_public_key, subject_action_id, receipt_created_at DESC);

CREATE INDEX idx_evidence_subject ON evidence(subject_kernel_public_key, subject_action_id, receipt_created_at DESC);

CREATE INDEX idx_grants_action ON grants(action_id);

CREATE INDEX idx_grants_connection ON grants(connection_id);

CREATE INDEX idx_idempotency ON idempotency_records(idempotency_key, counterparty_user_id);

CREATE UNIQUE INDEX idx_kernels_petname ON kernels(petname) WHERE petname IS NOT NULL;

CREATE UNIQUE INDEX idx_ledger_external_key ON ledger(external_key);

CREATE INDEX idx_ledger_from ON ledger(from_user_id);

CREATE INDEX idx_ledger_to ON ledger(to_user_id);

CREATE INDEX idx_pending_transfers_status ON pending_transfers(status);

CREATE INDEX idx_ratings_tx ON ratings(rated_tx_id);

CREATE INDEX idx_receipts_action_started ON receipts(action_id, started_at DESC);

CREATE INDEX idx_receipts_tx             ON receipts(tx_id);

CREATE INDEX idx_steps_required_caller ON steps(required_caller_user_id);

CREATE INDEX idx_steps_status          ON steps(status);

CREATE INDEX idx_traces_process_id ON traces(process_id);

CREATE INDEX idx_transactions_owner   ON transactions(owner_user_id);

CREATE INDEX idx_transactions_process ON transactions(process_id);

CREATE INDEX idx_transactions_trace   ON transactions(trace_id);
