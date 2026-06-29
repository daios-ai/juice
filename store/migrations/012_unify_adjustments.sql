-- Unify deposits and withdrawals into a single adjustments ledger.
-- direction distinguishes a credit grant from a debit redemption; external_key is
-- an optional, globally-unique idempotency token from the out-of-band payment system.
CREATE TABLE adjustments (
    id               TEXT PRIMARY KEY,
    operator_user_id TEXT NOT NULL REFERENCES users(id),
    target_user_id   TEXT NOT NULL REFERENCES users(id),
    direction        TEXT NOT NULL CHECK (direction IN ('credit','debit')),
    amount           INTEGER NOT NULL CHECK (amount > 0),
    reason           TEXT NOT NULL DEFAULT '',
    external_key     TEXT,
    created_at       TEXT NOT NULL
);

-- Unique when present; SQLite allows multiple NULLs, giving optional + unique-when-present.
CREATE UNIQUE INDEX idx_adjustments_external_key ON adjustments(external_key);

-- Preserve existing audit rows. UUIDs are unique across both tables, so no PK clash.
INSERT INTO adjustments (id,operator_user_id,target_user_id,direction,amount,reason,external_key,created_at)
    SELECT id,operator_user_id,target_user_id,'credit',amount,reason,NULL,created_at FROM deposits;
INSERT INTO adjustments (id,operator_user_id,target_user_id,direction,amount,reason,external_key,created_at)
    SELECT id,operator_user_id,target_user_id,'debit',amount,reason,NULL,created_at FROM withdrawals;

DROP TABLE deposits;
DROP TABLE withdrawals;
