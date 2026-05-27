-- Audit table for superuser-issued out-of-band deposits.
CREATE TABLE IF NOT EXISTS deposits (
    id               TEXT PRIMARY KEY,
    operator_user_id TEXT NOT NULL REFERENCES users(id),
    target_user_id   TEXT NOT NULL REFERENCES users(id),
    amount           INTEGER NOT NULL CHECK (amount > 0),
    reason           TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
);
