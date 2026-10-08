-- A kernel is a kernel and a user is a user (D4, D15). A peer kernel was an account row with no
-- handle, no credential and a balance zero on every path, kept so that every record naming a party
-- could name it by account id. Every such record now names a party as a principal — the kernel it
-- lives on, empty for this one, and the user there, empty for that kernel itself — and a peer is
-- its `kernels` row alone, which also carries its suspension. Only users hold accounts.
--
-- Nothing signed is rewritten (G3): a receipt keeps the account id it was signed with. A purged
-- peer's tombstone has no key left to name, so the rows that name it keep its id with no kernel; the
-- id matches no account and ids are never reused, so such a row renders as the raw id it always has,
-- and can neither authenticate nor match a user.

-- Party columns on traces are rewritten, so no cross-kernel call may be in doubt; and a database whose
-- ledger or processes name a peer predates every network served today, and is the operator's to keep.
CREATE TABLE migration_059_guard (
    parked_calls INTEGER NOT NULL
        CONSTRAINT "a call is still awaiting a peer's receipt: let it settle before upgrading"
        CHECK (parked_calls = 0),
    calls_in_flight INTEGER NOT NULL
        CONSTRAINT "a call from a peer has not finished: let it settle before upgrading"
        CHECK (calls_in_flight = 0),
    peer_money INTEGER NOT NULL
        CONSTRAINT "the ledger or a process names a peer kernel's account, which only a database older than every network served today holds"
        CHECK (peer_money = 0)
);

INSERT INTO migration_059_guard (parked_calls, calls_in_flight, peer_money)
SELECT
    (SELECT COUNT(*) FROM traces t
      WHERE t.idempotency_key IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id)),
    (SELECT COUNT(*) FROM idempotency_records r
      JOIN traces t ON t.idempotency_record_id = r.id
     WHERE NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id)),
    (SELECT COUNT(*) FROM ledger l
      WHERE l.operator_user_id IN (SELECT id FROM accounts WHERE handle IS NULL)
         OR l.from_user_id IN (SELECT id FROM accounts WHERE handle IS NULL)
         OR l.to_user_id IN (SELECT id FROM accounts WHERE handle IS NULL))
  + (SELECT COUNT(*) FROM processes p WHERE p.owner_user_id IN (SELECT id FROM accounts WHERE handle IS NULL));

DROP TABLE migration_059_guard;

-- A peer's suspension is its kernel's.
ALTER TABLE kernels ADD COLUMN suspended_at TEXT;
UPDATE kernels SET suspended_at = (SELECT a.suspended_at FROM accounts a WHERE a.kernel_public_key = kernels.public_key);

-- Traces and transactions: the remote user's id moves into the user column beside its kernel.
ALTER TABLE traces ADD COLUMN caller_kernel TEXT NOT NULL DEFAULT '';
ALTER TABLE traces ADD COLUMN target_kernel TEXT NOT NULL DEFAULT '';
UPDATE traces SET caller_kernel = (SELECT kernel_public_key FROM accounts WHERE id = traces.caller_user_id),
                  caller_user_id = caller_remote_id
 WHERE caller_user_id IN (SELECT id FROM accounts WHERE kernel_public_key IS NOT NULL);
UPDATE traces SET target_kernel = (SELECT kernel_public_key FROM accounts WHERE id = traces.action_owner_id),
                  action_owner_id = target_remote_id
 WHERE action_owner_id IN (SELECT id FROM accounts WHERE kernel_public_key IS NOT NULL);
ALTER TABLE traces DROP COLUMN caller_remote_id;
ALTER TABLE traces DROP COLUMN target_remote_id;

ALTER TABLE transactions ADD COLUMN caller_kernel TEXT NOT NULL DEFAULT '';
ALTER TABLE transactions ADD COLUMN target_kernel TEXT NOT NULL DEFAULT '';
UPDATE transactions SET caller_kernel = (SELECT kernel_public_key FROM accounts WHERE id = transactions.caller_user_id),
                        caller_user_id = caller_remote_id
 WHERE caller_user_id IN (SELECT id FROM accounts WHERE kernel_public_key IS NOT NULL);
UPDATE transactions SET target_kernel = (SELECT kernel_public_key FROM accounts WHERE id = transactions.target_user_id),
                        target_user_id = target_remote_id
 WHERE target_user_id IN (SELECT id FROM accounts WHERE kernel_public_key IS NOT NULL);
ALTER TABLE transactions DROP COLUMN caller_remote_id;
ALTER TABLE transactions DROP COLUMN target_remote_id;
CREATE INDEX idx_transactions_caller ON transactions(caller_kernel, caller_user_id);
CREATE INDEX idx_transactions_target ON transactions(target_kernel, target_user_id);
-- A proxy call's stored name was `owner/name`; the owner's handle is the target's.
UPDATE transactions SET target_handle = CASE WHEN target_handle = '' THEN substr(action_name, 1, instr(action_name, '/') - 1) ELSE target_handle END,
                        action_name = substr(action_name, instr(action_name, '/') + 1)
 WHERE remote_action_id <> '';

-- Tasks and the mailbox: one column holds local and remote ids, so it references no account.
CREATE TABLE tasks_059 (
    id                      TEXT PRIMARY KEY,
    parent_trace_id         TEXT REFERENCES traces(id),
    required_caller_kernel  TEXT NOT NULL DEFAULT '',
    required_caller_user_id TEXT NOT NULL,
    required_caller_handle  TEXT NOT NULL DEFAULT '',
    action_id               TEXT NOT NULL REFERENCES actions(id),
    partial_args            TEXT NOT NULL DEFAULT '{}',
    price                   INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    import_bps              INTEGER,
    status                  TEXT NOT NULL DEFAULT 'waiting'
                                CHECK(status IN ('waiting','running','done','cancelled')),
    tx_id                   TEXT,
    completion_trace_id     TEXT REFERENCES traces(id),
    revision                INTEGER NOT NULL DEFAULT 0,
    told_revision           INTEGER NOT NULL DEFAULT 0,
    told_failed_at          TEXT,
    created_at              TEXT NOT NULL
);
INSERT INTO tasks_059 (id, parent_trace_id, required_caller_kernel, required_caller_user_id, required_caller_handle,
                       action_id, partial_args, price, import_bps, status, tx_id, completion_trace_id,
                       revision, told_revision, told_failed_at, created_at)
SELECT s.id, s.parent_trace_id, COALESCE(a.kernel_public_key, ''),
       CASE WHEN a.kernel_public_key IS NULL THEN s.required_caller_user_id ELSE COALESCE(s.required_caller_remote_id, '') END,
       s.required_caller_handle, s.action_id, s.partial_args, s.price, s.import_bps, s.status, s.tx_id,
       s.completion_trace_id, s.revision, s.told_revision, s.told_failed_at, s.created_at
  FROM tasks s LEFT JOIN accounts a ON a.id = s.required_caller_user_id;
DROP TABLE tasks;
ALTER TABLE tasks_059 RENAME TO tasks;
CREATE INDEX idx_tasks_required_caller ON tasks(required_caller_kernel, required_caller_user_id);
CREATE INDEX idx_tasks_status          ON tasks(status);

CREATE TABLE mailbox_059 (
    holder_key              TEXT NOT NULL,
    id                      TEXT NOT NULL,
    required_caller_kernel  TEXT NOT NULL DEFAULT '',
    required_caller_user_id TEXT NOT NULL,
    required_caller_handle  TEXT NOT NULL DEFAULT '',
    owner_user_id           TEXT REFERENCES accounts(id),
    process_id              TEXT,
    status                  TEXT NOT NULL CHECK (status IN ('waiting','running','done','cancelled')),
    revision                INTEGER NOT NULL,
    notice_json             TEXT NOT NULL,
    created_at              TEXT NOT NULL,
    PRIMARY KEY (holder_key, id)
);
INSERT INTO mailbox_059 (holder_key, id, required_caller_kernel, required_caller_user_id, required_caller_handle,
                         owner_user_id, process_id, status, revision, notice_json, created_at)
SELECT m.holder_key, m.id, COALESCE(a.kernel_public_key, ''),
       CASE WHEN a.kernel_public_key IS NULL THEN m.required_caller_user_id ELSE COALESCE(m.required_caller_remote_id, '') END,
       m.required_caller_handle, m.owner_user_id, m.process_id, m.status, m.revision, m.notice_json, m.created_at
  FROM mailbox m LEFT JOIN accounts a ON a.id = m.required_caller_user_id;
DROP TABLE mailbox;
ALTER TABLE mailbox_059 RENAME TO mailbox;
CREATE UNIQUE INDEX idx_mailbox_id ON mailbox(id);
CREATE INDEX idx_mailbox_required_caller ON mailbox(required_caller_kernel, required_caller_user_id);
CREATE INDEX idx_mailbox_owner ON mailbox(owner_user_id);

-- Actions: a cached remote action is owned by the peer's user, on the peer, whose handle it keeps
-- beside its name rather than folded into it (`owner/name`, the only form a proxy was ever stored in).
CREATE TABLE actions_059 (
    id               TEXT PRIMARY KEY,
    owner_kernel     TEXT NOT NULL DEFAULT '',
    owner_user_id    TEXT NOT NULL,
    owner_handle     TEXT NOT NULL DEFAULT '',
    name             TEXT NOT NULL,
    title            TEXT NOT NULL DEFAULT '',
    kind             TEXT NOT NULL CHECK (kind IN ('http','wasm','native','remote_proxy')),
    active           INTEGER NOT NULL DEFAULT 0,
    visibility       TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private','local','public')),
    price            INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    base_price       INTEGER,
    remote_bps       INTEGER,
    effect           TEXT,
    description      TEXT NOT NULL DEFAULT '',
    input_schema     TEXT NOT NULL DEFAULT '{}',
    output_schema    TEXT NOT NULL DEFAULT '{}',
    source           TEXT NOT NULL DEFAULT '',
    artifact_hash    TEXT NOT NULL DEFAULT '',
    wasm_artifact    TEXT NOT NULL DEFAULT '',
    auth_json        TEXT NOT NULL DEFAULT '',
    embed_vec        TEXT,
    remote_action_id TEXT NOT NULL DEFAULT '',
    deleted_at       TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
INSERT INTO actions_059 (id, owner_kernel, owner_user_id, owner_handle, name, title, kind, active, visibility, price,
                         base_price, remote_bps, effect, description, input_schema, output_schema, source, artifact_hash,
                         wasm_artifact, auth_json, embed_vec, remote_action_id, deleted_at, created_at, updated_at)
SELECT x.id, COALESCE(a.kernel_public_key, ''),
       CASE WHEN a.kernel_public_key IS NULL THEN x.owner_user_id ELSE COALESCE(x.remote_owner_id, '') END,
       CASE WHEN x.kind = 'remote_proxy' THEN substr(x.name, 1, instr(x.name, '/') - 1) ELSE '' END,
       CASE WHEN x.kind = 'remote_proxy' THEN substr(x.name, instr(x.name, '/') + 1) ELSE x.name END,
       x.title, x.kind, x.active, x.visibility, x.price, x.base_price, x.remote_bps, x.effect,
       x.description, x.input_schema, x.output_schema, CASE WHEN x.kind = 'remote_proxy' THEN '' ELSE x.source END, x.artifact_hash, x.wasm_artifact, x.auth_json,
       x.embed_vec, x.remote_action_id, x.deleted_at, x.created_at, x.updated_at
  FROM actions x LEFT JOIN accounts a ON a.id = x.owner_user_id;
DROP TABLE actions;
ALTER TABLE actions_059 RENAME TO actions;
CREATE INDEX idx_actions_owner ON actions(owner_kernel, owner_user_id);
-- A user renamed on the peer who then replaced an action left two cached copies that are one name
-- once unfolded; the copy refreshed last stands, and the older is retired with its history (D13).
UPDATE actions SET active = 0, deleted_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
 WHERE kind = 'remote_proxy' AND deleted_at IS NULL
   AND EXISTS (SELECT 1 FROM actions n
                WHERE n.owner_kernel = actions.owner_kernel AND n.owner_user_id = actions.owner_user_id
                  AND n.name = actions.name AND n.deleted_at IS NULL AND n.id <> actions.id
                  AND (n.updated_at > actions.updated_at OR (n.updated_at = actions.updated_at AND n.id > actions.id)));
CREATE UNIQUE INDEX idx_actions_owner_name_active ON actions(owner_kernel, owner_user_id, name) WHERE deleted_at IS NULL;

-- An inbound call's lock is keyed by the kernel that signed it (P4). A lock whose kernel was purged
-- is no live lock: the guard above proved no inbound call is in doubt.
CREATE TABLE idempotency_records_059 (
    id              TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    counterparty    TEXT NOT NULL,
    args_json       TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    UNIQUE (idempotency_key, counterparty)
);
INSERT INTO idempotency_records_059 (id, idempotency_key, counterparty, args_json, created_at)
SELECT r.id, r.idempotency_key, a.kernel_public_key, r.args_json, r.created_at
  FROM idempotency_records r JOIN accounts a ON a.id = r.counterparty_user_id
 WHERE a.kernel_public_key IS NOT NULL;
DROP TABLE idempotency_records;
ALTER TABLE idempotency_records_059 RENAME TO idempotency_records;

-- Accounts are users. A balance is never negative, for every account there is.
CREATE TABLE accounts_059 (
    id                  TEXT PRIMARY KEY,
    handle              TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    password_hash       TEXT NOT NULL DEFAULT '',
    recovery_public_key TEXT,
    blockchain_address  TEXT,
    available           INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
    locked              INTEGER NOT NULL DEFAULT 0 CHECK (locked >= 0),
    suspended_at        TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);
INSERT INTO accounts_059 (id, handle, description, password_hash, recovery_public_key, blockchain_address,
                          available, locked, suspended_at, created_at, updated_at)
SELECT id, handle, description, password_hash, recovery_public_key, blockchain_address,
       available, locked, suspended_at, created_at, updated_at
  FROM accounts WHERE kernel_public_key IS NULL AND handle IS NOT NULL;
DROP TABLE accounts;
ALTER TABLE accounts_059 RENAME TO accounts;
CREATE UNIQUE INDEX idx_accounts_handle ON accounts(handle);
CREATE UNIQUE INDEX idx_accounts_blockchain_address ON accounts(blockchain_address) WHERE blockchain_address IS NOT NULL;
