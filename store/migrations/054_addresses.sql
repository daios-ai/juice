-- Every party is a principal, established once where a call enters the kernel and copied to the
-- transaction by every settlement path (D4): the local account that funds and routes, and — when
-- that account stands for a user on a peer — that user's stable id there and the handle they went
-- by. The caller of an inbound call is now signed into the request by its home kernel (P4), and a
-- step's completer into its completion (P8), so the seller records who called, not only from where.
--
-- A protocol epoch, not a rewrite of history: nothing committed is touched (G3). A transaction from
-- before it carries no remote half and renders the kernel it named, which is what was known. The
-- guard is 052's: no cross-kernel call may be in doubt when the epoch begins, since one crossing
-- the boundary would be signed under one rule and verified under the other.

CREATE TABLE migration_054_guard (
    parked_calls INTEGER NOT NULL
        CONSTRAINT "a call is still awaiting a peer's receipt: let it settle before upgrading"
        CHECK (parked_calls = 0),
    calls_in_flight INTEGER NOT NULL
        CONSTRAINT "a call from a peer has not finished: let it settle before upgrading"
        CHECK (calls_in_flight = 0)
);

INSERT INTO migration_054_guard (parked_calls, calls_in_flight)
SELECT
    (SELECT COUNT(*) FROM traces t
      WHERE t.idempotency_key IS NOT NULL
        AND NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id)),
    (SELECT COUNT(*) FROM idempotency_records r
      JOIN traces t ON t.idempotency_record_id = r.id
     WHERE NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id));

DROP TABLE migration_054_guard;

ALTER TABLE traces ADD COLUMN caller_remote_id TEXT NOT NULL DEFAULT '';
ALTER TABLE traces ADD COLUMN caller_handle    TEXT NOT NULL DEFAULT '';
ALTER TABLE traces ADD COLUMN target_remote_id TEXT NOT NULL DEFAULT '';
ALTER TABLE traces ADD COLUMN target_handle    TEXT NOT NULL DEFAULT '';

ALTER TABLE transactions ADD COLUMN caller_remote_id TEXT NOT NULL DEFAULT '';
ALTER TABLE transactions ADD COLUMN caller_handle    TEXT NOT NULL DEFAULT '';
ALTER TABLE transactions ADD COLUMN target_remote_id TEXT NOT NULL DEFAULT '';
ALTER TABLE transactions ADD COLUMN target_handle    TEXT NOT NULL DEFAULT '';
