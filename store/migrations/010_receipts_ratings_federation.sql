-- Federation: receipts, ratings table, idempotency records, remote kernel user fields.

ALTER TABLE users ADD COLUMN public_key TEXT;
ALTER TABLE users ADD COLUMN remote_base_url TEXT;

ALTER TABLE transactions ADD COLUMN remote_receipt_hash TEXT;

-- receipts: immutable signed record for every committed call.
CREATE TABLE IF NOT EXISTS receipts (
    id              TEXT PRIMARY KEY,
    issuer_user_id  TEXT NOT NULL REFERENCES users(id),
    tx_id           TEXT NOT NULL UNIQUE REFERENCES transactions(id),
    trace_id        TEXT NOT NULL,
    action_id       TEXT NOT NULL,
    args_hash       TEXT NOT NULL DEFAULT '',
    reply_hash      TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL,
    gross           INTEGER NOT NULL DEFAULT 0,
    net             INTEGER NOT NULL DEFAULT 0,
    fee             INTEGER NOT NULL DEFAULT 0,
    reason          TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    signature       TEXT NOT NULL DEFAULT ''
);

-- ratings: separate table replacing the deprecated transactions.rating column.
CREATE TABLE IF NOT EXISTS ratings (
    id               TEXT PRIMARY KEY,
    rated_tx_id      TEXT NOT NULL UNIQUE REFERENCES transactions(id),
    rated_receipt_id TEXT REFERENCES receipts(id),
    rater_user_id    TEXT NOT NULL REFERENCES users(id),
    rating           REAL NOT NULL CHECK (rating IN (0, 1)),
    created_at       TEXT NOT NULL,
    signature        TEXT NOT NULL DEFAULT ''
);

-- idempotency_records: prevent duplicate cross-kernel calls.
CREATE TABLE IF NOT EXISTS idempotency_records (
    id                   TEXT PRIMARY KEY,
    idempotency_key      TEXT NOT NULL,
    counterparty_user_id TEXT NOT NULL REFERENCES users(id),
    receipt_id           TEXT REFERENCES receipts(id),
    created_at           TEXT NOT NULL,
    expires_at           TEXT NOT NULL,
    UNIQUE (idempotency_key, counterparty_user_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_public_key ON users(public_key) WHERE public_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_receipts_tx ON receipts(tx_id);
CREATE INDEX IF NOT EXISTS idx_ratings_tx ON ratings(rated_tx_id);
CREATE INDEX IF NOT EXISTS idx_idempotency ON idempotency_records(idempotency_key, counterparty_user_id);
