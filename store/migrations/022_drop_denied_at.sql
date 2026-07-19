-- Drop the peer `denied_at` column (§13). Federation moved from bilateral friend/deny to directional
-- subscribe + unified suspend: a blocked peer is now just a suspended account (`suspended_at`), so the
-- separate deny axis is gone. SQLite has no DROP COLUMN with constraints, so rebuild users (pattern of
-- 005/017/018/021). Any prior denial is intentionally not carried over — re-express it with `suspend`.
CREATE TABLE users_new (
    id                  TEXT PRIMARY KEY,
    handle              TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    password_hash       TEXT NOT NULL,
    available           INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked              INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    suspended_at        TEXT,
    public_key          TEXT,
    recovery_public_key TEXT,
    peer_last_seen      TEXT,
    peer_credit         INTEGER,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);
INSERT INTO users_new (id,handle,description,password_hash,available,locked,suspended_at,public_key,recovery_public_key,peer_last_seen,peer_credit,created_at,updated_at)
    SELECT id,handle,description,password_hash,available,locked,suspended_at,public_key,recovery_public_key,peer_last_seen,peer_credit,created_at,updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE UNIQUE INDEX idx_users_public_key ON users(public_key) WHERE public_key IS NOT NULL;
