-- The value channel becomes local to one kernel: value between kernels settles on the external rail,
-- not through a call. This drops what only the cross-kernel legs used — the buyer-side payment reserve,
-- the trace reserve column that only cross-kernel fees made differ from the delivered value, and the
-- manifest effect mirror on discovery docs.
--
-- Money is never dropped with the records that back it. The guards below abort the whole migration
-- (it runs in one transaction) while any locked value would be stranded, so the kernel refuses to
-- start until the operator drains it: settle or refund the pending transfers, then let the affected
-- calls settle or force their processes closed. Receipts keep every value field — they are immutable
-- signed records, and older ones must stay verifiable.

CREATE TABLE migration_043_guard (
    unresolved INTEGER NOT NULL
        CONSTRAINT "unresolved cross-kernel value transfers: settle or refund them before upgrading"
        CHECK (unresolved = 0),
    stranded INTEGER NOT NULL
        CONSTRAINT "calls still hold a cross-kernel value reserve: let them settle before upgrading"
        CHECK (stranded = 0)
);

INSERT INTO migration_043_guard (unresolved, stranded)
SELECT
    (SELECT COUNT(*) FROM pending_transfers WHERE status IN ('pending', 'quarantined')),
    -- An unsettled value lock that local-only settlement cannot release correctly. The shape decides,
    -- never the amounts: fees can be configured to zero, so a cross-kernel lock may look exactly like
    -- a local one. Three shapes strand: a reserve enlarged by cross-kernel fees (settlement would
    -- release the wrong amount), an outbound transfer with no local beneficiary (nothing to deliver
    -- to), and value drawn from a peer's row (a peer funds no transfer now). A plain local transfer —
    -- ordinary caller, local beneficiary, reserve equal to the value — releases identically once the
    -- column is gone. Unsettled = no transaction has resolved the trace.
    (SELECT COUNT(*) FROM traces t
      WHERE (COALESCE(t.value_reserve, 0) > 0 OR COALESCE(t.value, 0) > 0)
        AND NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id)
        AND (COALESCE(t.value_reserve, 0) <> COALESCE(t.value, 0)
             OR COALESCE(t.value_to, '') = ''
             OR EXISTS (SELECT 1 FROM accounts a
                         WHERE a.id = t.caller_user_id AND a.kernel_public_key IS NOT NULL)));

DROP TABLE migration_043_guard;

DROP INDEX idx_pending_transfers_status;

DROP TABLE pending_transfers;

ALTER TABLE traces DROP COLUMN value_reserve;

ALTER TABLE discovery_docs DROP COLUMN effect;

-- A proxy is never value-bearing now that no manifest declares an effect. Expected to match nothing;
-- a row that somehow carries one is disarmed and deactivated so the next call re-resolves it.
UPDATE actions SET effect = '', active = 0 WHERE kind = 'remote_proxy' AND effect <> '';
