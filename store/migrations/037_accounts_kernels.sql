-- v0.12 account/kernel split (§3, §13). A local user and a remote kernel are different entities
-- that both hold an account, so `users` becomes `accounts` (the ledger principal) and a new
-- `kernels` table owns remote-kernel identity and Stiegler naming state (key / nickname / petname).
-- `discovered_kernels` is absorbed: a kernel is one row whether or not it has an account.
-- Account ids are PRESERVED, so every captured *_user_id in transactions, receipts, steps,
-- processes, traces, actions, ledger, and idempotency_records stays valid — a schema split, not a
-- history migration.
--
-- NOTE: statement comments must be on their own lines (the migration splitter carries an inline
-- trailing comment into the next statement and drops it), so every note here is a full-line comment.
--
-- kernels: one row per known kernel. petname is the LOCAL name (assigned here, resolves);
-- nickname is the kernel's self-asserted label carried in gossip (never resolves). Field
-- precedence when a kernel exists in both sources: petname from the existing peer account handle
-- (the operator's already-bound name, never discarded), nickname/about/gossip_cursor from the
-- discovery row, last_seen/peer_credit from the peer account, first_seen earliest and updated_at
-- latest of the two.
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

INSERT INTO kernels (public_key, petname, nickname, about, gossip_cursor, last_seen, peer_credit, first_seen, updated_at)
    SELECT k.public_key,
           u.handle,
           COALESCE(d.handle, ''),
           COALESCE(d.about, ''),
           COALESCE(d.gossip_cursor, ''),
           u.peer_last_seen,
           u.peer_credit,
           MIN(COALESCE(u.created_at, d.first_seen), COALESCE(d.first_seen, u.created_at)),
           MAX(COALESCE(u.updated_at, d.updated_at), COALESCE(d.updated_at, u.updated_at))
      FROM (SELECT public_key FROM users WHERE public_key IS NOT NULL AND public_key != ''
            UNION
            SELECT public_key FROM discovered_kernels) k
      LEFT JOIN users u ON u.public_key = k.public_key
      LEFT JOIN discovered_kernels d ON d.public_key = k.public_key;

-- Rename first so SQLite rewrites every child table's REFERENCES users(id) to REFERENCES
-- accounts(id) (legacy_alter_table is off by default). A create-new/drop-old rebuild alone would
-- leave those child definitions pointing at a table that no longer exists, and the runner disables
-- FK enforcement per migration, so the breakage would be silent.
ALTER TABLE users RENAME TO accounts;

-- Final account schema: handle is nullable (a kernel account has none), the peer discriminator is
-- now a foreign key to kernels rather than a bare credential string, and the third CHECK makes the
-- separation real — a kernel account can never hold a session credential. A legacy row holding both
-- a password and a key violates it, failing the migration rather than silently discarding data.
CREATE TABLE accounts_new (
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

INSERT INTO accounts_new (id,handle,description,password_hash,recovery_public_key,available,locked,suspended_at,kernel_public_key,created_at,updated_at)
    SELECT id,
           CASE WHEN public_key IS NOT NULL AND public_key != '' THEN NULL ELSE handle END,
           description, password_hash, recovery_public_key, available, locked, suspended_at,
           CASE WHEN public_key != '' THEN public_key END,
           created_at, updated_at
      FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;

-- Uniqueness lives only in partial indexes: an inline UNIQUE on a nullable column would build a
-- duplicate index and obscure the nullable semantics. idx_accounts_kernel is what enforces at most
-- one account per kernel; the foreign key needs only the parent key, which is already the PK.
CREATE UNIQUE INDEX idx_accounts_handle ON accounts(handle) WHERE handle IS NOT NULL;
CREATE UNIQUE INDEX idx_accounts_kernel ON accounts(kernel_public_key) WHERE kernel_public_key IS NOT NULL;
CREATE UNIQUE INDEX idx_kernels_petname ON kernels(petname) WHERE petname IS NOT NULL;

DROP TABLE discovered_kernels;
