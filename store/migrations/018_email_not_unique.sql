-- Stop enforcing email uniqueness: §3 never required it (only handle/public_key are unique),
-- so the DB was stricter than the spec. SQLite has no DROP CONSTRAINT and the inline UNIQUE is
-- backed by an implicit autoindex, so drop it by rebuilding users (same pattern as 005/017).
-- email stays NOT NULL — still required, just no longer unique.
CREATE TABLE users_new (
    id             TEXT PRIMARY KEY,
    handle         TEXT NOT NULL UNIQUE,
    email          TEXT NOT NULL,
    password_hash  TEXT NOT NULL,
    available      INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked         INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    suspended_at   TEXT,
    denied_at      TEXT,
    public_key     TEXT,
    peer_last_seen TEXT,
    peer_credit    INTEGER,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);
INSERT INTO users_new (id,handle,email,password_hash,available,locked,suspended_at,denied_at,public_key,peer_last_seen,peer_credit,created_at,updated_at)
    SELECT id,handle,email,password_hash,available,locked,suspended_at,denied_at,public_key,peer_last_seen,peer_credit,created_at,updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE UNIQUE INDEX idx_users_public_key ON users(public_key) WHERE public_key IS NOT NULL;
