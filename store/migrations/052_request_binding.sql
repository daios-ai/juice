-- A receipt names the request it answers, a lock exists only while work may be running, and
-- evidence eligibility is a fact of the trade (P4, P5, P9).
--
-- This is a protocol epoch, not a rewrite of history. Nothing already signed is touched: an old
-- receipt keeps the exact bytes its signature covers and the exact hash the other kernel holds for
-- it, and reads back as unbound. What the new rule needs instead is that no cross-kernel call is
-- in doubt when the epoch begins, because a call that crosses the boundary would be answered under
-- one rule and settled under the other. The guard aborts the whole migration (it runs in one
-- transaction) while any is, so the kernel refuses to start until the operator has let them
-- settle.

CREATE TABLE migration_052_guard (
    parked_calls INTEGER NOT NULL
        CONSTRAINT "a call is still awaiting a peer's receipt: let it settle before upgrading"
        CHECK (parked_calls = 0),
    calls_in_flight INTEGER NOT NULL
        CONSTRAINT "a call from a peer has not finished: let it settle before upgrading"
        CHECK (calls_in_flight = 0)
);

INSERT INTO migration_052_guard (parked_calls, calls_in_flight)
SELECT
    -- Ours, awaiting an answer. Its retry after the upgrade would carry a key the peer's old
    -- receipt does not name, so the peer would execute it a second time.
    (SELECT COUNT(*) FROM traces t
      WHERE t.idempotency_key IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id)),
    -- Theirs, admitted and unsettled. Its terms were frozen before a receipt named the request, so
    -- the receipt recovery would sign for it is one the buyer would now refuse.
    (SELECT COUNT(*) FROM idempotency_records r
      JOIN traces t ON t.idempotency_record_id = r.id
     WHERE NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id));

DROP TABLE migration_052_guard;

-- The two fields the receipt now signs, projected into columns so a replay is one indexed read.
-- Empty on every receipt signed before this epoch: that is what unbound means, and it is why the
-- columns are never backfilled — the binding is not recoverable without changing the bytes the
-- signature covers and the hash the counterparty stored (G3).
ALTER TABLE receipts ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT '';
ALTER TABLE receipts ADD COLUMN counterparty TEXT NOT NULL DEFAULT '';
-- One receipt per request. Partial, because every local and every pre-epoch receipt answers no
-- request and carries an empty key; a plain unique index would admit exactly one of them.
CREATE UNIQUE INDEX IF NOT EXISTS idx_receipts_request ON receipts(counterparty, idempotency_key)
    WHERE idempotency_key <> '';

-- The record becomes a lock: it exists while execution may be running and is deleted by the commit
-- that writes the receipt. The guard above proved none is in flight, so every remaining row is
-- finished business and none is carried over.
CREATE TABLE idempotency_records_new (
    id                   TEXT PRIMARY KEY,
    idempotency_key      TEXT NOT NULL,
    counterparty_user_id TEXT NOT NULL REFERENCES "accounts"(id),
    args_json            TEXT NOT NULL DEFAULT '',
    created_at           TEXT NOT NULL,
    UNIQUE (idempotency_key, counterparty_user_id)
);
DROP TABLE idempotency_records;
ALTER TABLE idempotency_records_new RENAME TO idempotency_records;

-- Whether a call's receipt is evidence is decided when the call settles, from the whole rule, and
-- never re-derived from the action's current row (P9). A trade that happened before this epoch
-- carries no such decision, and the action as it stands today cannot supply one: an action public
-- now may have been private when it ran. Those trades stay ineligible, so no upgrade ever
-- publishes what may have been private business.
ALTER TABLE transactions ADD COLUMN evidence_eligible INTEGER NOT NULL DEFAULT 0;

-- A reveal the seller could not take is tried again after the ones never tried, so a peer that
-- cannot answer stops holding up everything behind it (P10).
ALTER TABLE traces ADD COLUMN reveal_failed_at TEXT;

-- A catalogue is read in pages and swept once the scan completes, so a large one converges instead
-- of losing its first page before its last arrives (P9).
ALTER TABLE kernels ADD COLUMN catalog_cursor TEXT NOT NULL DEFAULT '';
ALTER TABLE kernels ADD COLUMN catalog_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE discovery_docs ADD COLUMN generation INTEGER NOT NULL DEFAULT 0;
