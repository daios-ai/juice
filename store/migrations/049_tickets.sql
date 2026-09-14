-- Cross-kernel obligations stop accumulating as bilateral debt and settle per call by a lottery
-- ticket. What the old economy needed goes: the peer row that held a balance, the reserve column
-- that parked a serving markup, and the cached credit a peer reported. What replaces it is one
-- trace carrying the seller's side of each draw beside its frozen terms, and one number for what
-- this kernel is owed in total.
--
-- Money is never dropped with the records that back it. The guards abort the whole migration (it
-- runs in one transaction) while any old-world value is still in flight, so the kernel refuses to
-- start until the operator drains it: let the parked calls settle or force their processes closed,
-- finish the settlements already drawn for, and zero the peer rows.

CREATE TABLE migration_049_guard (
    peer_balances INTEGER NOT NULL
        CONSTRAINT "peer accounts still hold a balance: settle every position before upgrading"
        CHECK (peer_balances = 0),
    parked_calls INTEGER NOT NULL
        CONSTRAINT "calls are still awaiting a remote receipt: let them settle before upgrading"
        CHECK (parked_calls = 0),
    open_settlements INTEGER NOT NULL
        CONSTRAINT "settlements are still open: finish or expire them before upgrading"
        CHECK (open_settlements = 0)
);

INSERT INTO migration_049_guard (peer_balances, parked_calls, open_settlements)
SELECT
    -- A peer row is about to become identity alone, so anything it still holds would be money with
    -- no record: a prepaid credit nobody could spend, or a debt nobody could collect.
    (SELECT COUNT(*) FROM accounts
      WHERE kernel_public_key IS NOT NULL AND (available <> 0 OR locked <> 0)),
    -- An unsettled call whose funds the dropped column backs. Two shapes: an outbound dispatch
    -- awaiting its receipt (its allocation is locked against a settlement that would now read a
    -- different economy), and an inbound call holding a serving-markup reserve in premium_parked —
    -- the column this migration drops, so nothing would release it afterwards.
    (SELECT COUNT(*) FROM traces t
      WHERE NOT EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = t.id)
        AND (t.idempotency_key IS NOT NULL OR COALESCE(t.premium_parked, 0) <> 0)),
    -- A residual settlement still mid-flight: being drawn for, or paid and not yet through. The
    -- protocol that would finish it is gone after this migration. A settlement the creditor has
    -- been told about, or one already credited or failed, is finished history and passes.
    (SELECT COUNT(*) FROM rail_transfers
      WHERE kind IN ('settlement', 'claim')
        AND status NOT IN ('failed', 'credited', 'announced'));

DROP TABLE migration_049_guard;

-- What this kernel has delivered and not been paid for, as one signed number. It rises when work is
-- admitted, corrects to the actual charge when the work commits, and falls only when cash arrives —
-- a losing draw discharges the obligation but not the exposure, which is what makes minting
-- identities pointless. It may go negative: premium income is what pays for the losses.
CREATE TABLE exposure (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    value INTEGER NOT NULL DEFAULT 0
);

INSERT INTO exposure (id, value) VALUES (1, 0);

-- The stake a call holds from its caller's own balance so a winning ticket is funded when it lands,
-- and whether the seller has acknowledged how the draw came out. Both belong to the dispatch, which
-- is what the trace already records.
--
-- Every call that existed before this upgrade is marked acknowledged. None of them drew for
-- anything: they carry no secret, and the peers they were made to hold no receivable to reveal
-- against, so leaving them unacknowledged would queue a reveal that fails forever and, being the
-- oldest, starves every real one behind it.
ALTER TABLE traces ADD COLUMN ticket INTEGER NOT NULL DEFAULT 0 CHECK (ticket >= 0);
ALTER TABLE traces ADD COLUMN revealed INTEGER NOT NULL DEFAULT 1;

-- The selling side of the same draw. An obligation is never a row of its own: the trace froze the
-- terms at admission, the receipt says what was charged, and these four carry the buyer's reveal —
-- how the draw came out, what moves, the payment named and the proven sender it comes from. The
-- outcome is read from `owed_amount`: nothing is a losing draw, the obligation an exact one,
-- anything else a win.
ALTER TABLE traces ADD COLUMN owed_status TEXT NOT NULL DEFAULT '' CHECK (owed_status IN ('', 'announced', 'cancelled', 'credited'));
ALTER TABLE traces ADD COLUMN owed_amount INTEGER NOT NULL DEFAULT 0 CHECK (owed_amount >= 0);
ALTER TABLE traces ADD COLUMN owed_tx_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE traces ADD COLUMN owed_rail_address TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_traces_owed ON traces(owed_status) WHERE owed_status <> '';

-- The serving markup is no longer parked: a foreign call is funded by the seller, not by the buyer's
-- row, so there is nothing to reserve against it.
ALTER TABLE traces DROP COLUMN premium_bps;
ALTER TABLE traces DROP COLUMN premium_parked;

-- A peer reported balance was a display cache of a number that no longer exists.
ALTER TABLE kernels DROP COLUMN peer_credit;

-- A won draw on its way to the peer it is owed to. It is kept apart from a withdrawal because the
-- two fail differently: a withdrawal the rail rejects gives its money back, while a debt that failed
-- to pay is still a debt, so the money stays committed and the payment is presented again — under a
-- fresh `attempt`, since a rail operation that has been signed and settled cannot be reused.
ALTER TABLE rail_transfers RENAME TO rail_transfers_old;
CREATE TABLE rail_transfers (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('deposit','payout','obligation','settlement','claim','refill')),
    party        TEXT NOT NULL DEFAULT '',
    amount       INTEGER NOT NULL CHECK (amount > 0),
    credit       INTEGER NOT NULL DEFAULT 0 CHECK (credit >= 0 AND credit <= amount),
    destination  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('drawing','pending','submitted','refilling','blocked','confirmed','failed','announced','held','credited')),
    tx_hash      TEXT NOT NULL DEFAULT '',
    refill_id    TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    attempt      INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    created_at   TEXT NOT NULL,
    finalized_at TEXT
);
INSERT INTO rail_transfers (id,kind,party,amount,credit,destination,status,tx_hash,refill_id,reason,created_at,finalized_at)
SELECT id,kind,party,amount,credit,destination,status,tx_hash,refill_id,reason,created_at,finalized_at FROM rail_transfers_old;
DROP TABLE rail_transfers_old;
CREATE INDEX idx_rail_transfers_open ON rail_transfers(kind, status);
CREATE INDEX idx_rail_transfers_party ON rail_transfers(party);

-- The serving kernel's half of the settlement draw, signed into the receipt that carries the
-- obligation. A receipt written before the draw existed carries none, and owes nothing to draw for.
ALTER TABLE receipts ADD COLUMN nonce TEXT NOT NULL DEFAULT '';
