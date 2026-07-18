-- Retire email and add recovery/description to users (§3, §12). Email had no recovery flow and was
-- never unique-required, so it is dead weight; drop it. Add `description` (free-text "about"; @sys's
-- is the kernel's about, §13) and `recovery_public_key` (the account's own Ed25519 recovery key,
-- base64url, enrolled from a client-held seed phrase — distinct from public_key so it never makes the
-- account a peer). SQLite has no DROP COLUMN with constraints, so rebuild users (pattern of 005/017/018).
CREATE TABLE users_new (
    id                  TEXT PRIMARY KEY,
    handle              TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    password_hash       TEXT NOT NULL,
    available           INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked              INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    suspended_at        TEXT,
    denied_at           TEXT,
    public_key          TEXT,
    recovery_public_key TEXT,
    peer_last_seen      TEXT,
    peer_credit         INTEGER,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);
INSERT INTO users_new (id,handle,description,password_hash,available,locked,suspended_at,denied_at,public_key,recovery_public_key,peer_last_seen,peer_credit,created_at,updated_at)
    SELECT id,handle,'',password_hash,available,locked,suspended_at,denied_at,public_key,NULL,peer_last_seen,peer_credit,created_at,updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE UNIQUE INDEX idx_users_public_key ON users(public_key) WHERE public_key IS NOT NULL;

-- Single-use, TTL-bound nonces for the password-recovery challenge (§12). Consumed atomically by a
-- DELETE ... RETURNING, so a captured recovery request cannot be replayed.
CREATE TABLE recovery_challenge (
    nonce      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
