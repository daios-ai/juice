-- Generalize the adjustments ledger into a from/to ledger that represents every
-- direct balance movement as one row: a deposit credits (from NULL -> to user), a
-- withdrawal debits (from user -> to NULL), and a user transfer moves between two
-- local users (from sender -> to recipient). operator_user_id is the authorizer:
-- @sys for a deposit/withdrawal, the sender for a transfer.
CREATE TABLE ledger (
    id               TEXT PRIMARY KEY,
    operator_user_id TEXT NOT NULL REFERENCES users(id),
    from_user_id     TEXT REFERENCES users(id),
    to_user_id       TEXT REFERENCES users(id),
    amount           INTEGER NOT NULL CHECK (amount > 0),
    reason           TEXT NOT NULL DEFAULT '',
    external_key     TEXT,
    created_at       TEXT NOT NULL,
    CHECK (from_user_id IS NOT NULL OR to_user_id IS NOT NULL)
);

-- Unique when present; SQLite allows multiple NULLs, giving optional + unique-when-present.
CREATE UNIQUE INDEX idx_ledger_external_key ON ledger(external_key);
CREATE INDEX idx_ledger_from ON ledger(from_user_id);
CREATE INDEX idx_ledger_to ON ledger(to_user_id);

-- Preserve existing audit rows. A credit becomes an inbound (to) entry, a debit an
-- outbound (from) entry; the same UUIDs carry over so no PK clash.
INSERT INTO ledger (id,operator_user_id,from_user_id,to_user_id,amount,reason,external_key,created_at)
    SELECT id,operator_user_id,NULL,target_user_id,amount,reason,external_key,created_at
      FROM adjustments WHERE direction='credit';
INSERT INTO ledger (id,operator_user_id,from_user_id,to_user_id,amount,reason,external_key,created_at)
    SELECT id,operator_user_id,target_user_id,NULL,amount,reason,external_key,created_at
      FROM adjustments WHERE direction='debit';

DROP TABLE adjustments;
