-- v0.12 remote payment Step (§13): a buyer funding a value transfer attached to a Step hosted on
-- another kernel is NOT executing a local Call, so it must not fabricate a process/trace to hold the
-- reserve. pending_transfers is a dedicated reserve holder on the buyer side: max_total is locked from
-- the buyer's own balance reserve-first at admission and released to the peer proxy row + sys + refund
-- on the serving kernel's signed receipt, refunded on a valid failure, or kept LOCKED (quarantined) on
-- an invalid/inconsistent receipt after a possibly-executed dispatch (operator reconciliation).
-- `input` holds the raw completion input bytes (not just their hash) so a retry can rebuild the SAME
-- signed completion request; `remote_max` is the descriptor obligation settlement re-validates against;
-- `last_error` records the disposition reason (e.g. why a record was quarantined) for the operator;
-- `updated_at` advances on every status transition.
-- NOTE: full-line comments only (the migration splitter drops inline trailing comments).
CREATE TABLE pending_transfers (
    id              TEXT PRIMARY KEY,
    buyer_id        TEXT NOT NULL REFERENCES users(id),
    peer_key        TEXT NOT NULL,
    step_id         TEXT NOT NULL,
    input_hash      TEXT NOT NULL,
    input           TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    beneficiary     TEXT NOT NULL,
    amount          INTEGER NOT NULL,
    remote_max      INTEGER NOT NULL,
    reserve         INTEGER NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    last_error      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);
-- Sweeper / retry scan the still-locked records by status.
CREATE INDEX idx_pending_transfers_status ON pending_transfers(status);
