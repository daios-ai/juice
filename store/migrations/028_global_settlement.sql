-- v0.13 global settlement model (§3, §13): replace per-peer bilateral credit with a kernel-global
-- exposure cap X enforced at admission, and add the serving-markup premium leg to receipts.
-- NOTE: statement comments must be on their own lines (the migration splitter carries an inline
-- trailing comment into the next statement and drops it), so every note here is a full-line comment.
--
-- Rebuild `users` (021/022/027 rebuild pattern) to drop the per-peer credit policy columns
-- (peer_credit_max, peer_settlement_trigger, peer_settlement_due) and relax the row CHECK to the
-- global form: an ordinary account (no public_key) is still non-negative, while a peer account may
-- go negative — bounded not per-row but globally by X at admission (§13), so no per-row floor.
-- peer_last_seen and peer_credit (display-only sync cache) are retained.
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
    created_at              TEXT NOT NULL,
    updated_at              TEXT NOT NULL,
    CHECK (public_key IS NOT NULL OR available >= 0)
);
INSERT INTO users_new (id,handle,description,password_hash,available,locked,suspended_at,public_key,recovery_public_key,peer_last_seen,peer_credit,created_at,updated_at)
    SELECT id,handle,description,password_hash,available,locked,suspended_at,public_key,recovery_public_key,peer_last_seen,peer_credit,created_at,updated_at FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE UNIQUE INDEX idx_users_public_key ON users(public_key) WHERE public_key IS NOT NULL;
-- Serving-markup premium leg on receipts (§13): the amount the serving kernel charges its origin
-- peer on top of the base charge, credited to the serving kernel's sys. 0 on all local/pre-v0.13
-- receipts, so JCS verification of old receipts is unchanged (omitempty on the struct).
ALTER TABLE receipts ADD COLUMN premium INTEGER NOT NULL DEFAULT 0;
