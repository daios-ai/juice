-- v0.12 global principals + bilateral peer credit (§3, §8, §13).
-- NOTE: statement comments must be on their own lines (the migration splitter carries an inline
-- trailing comment into the next statement and drops it), so every note here is a full-line comment.
--
-- Principals (W6/W8): a remote action's/step's stable identity carried beneath the friendly name.
-- actions.remote_owner_id = stable owner user_id on the serving kernel.
ALTER TABLE actions ADD COLUMN remote_owner_id TEXT;
-- actions.remote_bps = provider premium snapshot from the signed manifest (NULL = pre-v0.12 row).
ALTER TABLE actions ADD COLUMN remote_bps INTEGER;
-- steps.required_caller_remote_id = stable remote user_id of a peer completer.
ALTER TABLE steps ADD COLUMN required_caller_remote_id TEXT;
-- Premium snapshot on inbound federated root traces (settlement math is config-change-proof).
ALTER TABLE traces ADD COLUMN premium_bps INTEGER;
ALTER TABLE traces ADD COLUMN premium_parked INTEGER;
-- Credit (W10): rebuild `users` (021/022 rebuild pattern) to relax the hard non-negativity CHECK to
-- the conditional bilateral bound `available >= -peer_credit_max` (ordinary accounts have
-- peer_credit_max=0, so `available >= 0` is preserved) and add provider-side per-peer credit policy
-- (peer_credit_max, peer_settlement_trigger) plus a debtor-side sync-cache flag (peer_settlement_due).
CREATE TABLE users_new (
    id                      TEXT PRIMARY KEY,
    handle                  TEXT NOT NULL UNIQUE,
    description             TEXT NOT NULL DEFAULT '',
    password_hash           TEXT NOT NULL,
    available               INTEGER NOT NULL DEFAULT 0,
    locked                  INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    suspended_at            TEXT,
    public_key              TEXT,
    recovery_public_key     TEXT,
    peer_last_seen          TEXT,
    peer_credit             INTEGER,
    peer_credit_max         INTEGER NOT NULL DEFAULT 0 CHECK (peer_credit_max >= 0),
    peer_settlement_trigger INTEGER NOT NULL DEFAULT 0,
    peer_settlement_due     INTEGER,
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL,
    CHECK (available >= -peer_credit_max)
);
INSERT INTO users_new (id,handle,description,password_hash,available,locked,suspended_at,public_key,recovery_public_key,peer_last_seen,peer_credit,created_at,updated_at)
    SELECT id,handle,description,password_hash,available,locked,suspended_at,public_key,recovery_public_key,peer_last_seen,peer_credit,created_at,updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE UNIQUE INDEX idx_users_public_key ON users(public_key) WHERE public_key IS NOT NULL;
