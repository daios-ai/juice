-- Rail (D23). Accounts and kernels record where they are paid; every external movement — a payment
-- in, a withdrawal, a settlement, a peer's claim, a fuel lock — becomes one row keyed by the fact
-- that caused it, so booking the same fact twice is impossible however it was found, and the money
-- in transit is one query rather than a scan of several tables.

ALTER TABLE accounts ADD COLUMN rail_address TEXT;
CREATE UNIQUE INDEX idx_accounts_rail_address ON accounts(rail_address) WHERE rail_address IS NOT NULL;

ALTER TABLE kernels ADD COLUMN rail_address TEXT NOT NULL DEFAULT '';
ALTER TABLE kernels ADD COLUMN rail_proof TEXT NOT NULL DEFAULT '';

CREATE TABLE rail_transfers (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('deposit','payout','settlement','claim','refill')),
    party        TEXT NOT NULL DEFAULT '',
    amount       INTEGER NOT NULL CHECK (amount > 0),
    credit       INTEGER NOT NULL DEFAULT 0 CHECK (credit >= 0 AND credit <= amount),
    destination  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('drawing','pending','submitted','refilling','blocked','confirmed','failed','announced','held','credited')),
    tx_hash      TEXT NOT NULL DEFAULT '',
    refill_id    TEXT NOT NULL DEFAULT '',
    record       TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    finalized_at TEXT
);

CREATE INDEX idx_rail_transfers_open ON rail_transfers(kind, status);
CREATE INDEX idx_rail_transfers_party ON rail_transfers(party);

-- A settlement closed under the retired cash record is history, not work: its payment is final and
-- its debt is gone. It becomes the row the rail table would hold for it — cash Q from the `.cash`
-- record, debt d from the companion outcome record, and the side from whether this kernel is the
-- record's creditor — in a terminal state, so the worker never drives it again.
INSERT INTO rail_transfers (id, kind, party, amount, credit, status, record, created_at, finalized_at)
SELECT s.sid,
       CASE s.ours WHEN 1 THEN 'claim' ELSE 'settlement' END,
       s.party, s.cash, s.debt,
       CASE s.ours WHEN 1 THEN 'credited' ELSE 'announced' END,
       s.record, s.created_at, s.created_at
  FROM (SELECT substr(c.external_key, 1, length(c.external_key) - 5) AS sid,
               c.to_user_id AS party, c.amount AS cash, p.amount AS debt,
               p.reason AS record, c.created_at AS created_at,
               json_extract(p.reason, '$.creditor')
                   = (SELECT value FROM config WHERE key = 'signing_public_key') AS ours
          FROM ledger c
          JOIN ledger p ON p.external_key = substr(c.external_key, 1, length(c.external_key) - 5)
         WHERE c.external_key LIKE '%.cash') s;

-- That record is also where money crossed the rail, so the solvency audit must count it. Give it
-- the shape of a crossing — one side null, in the direction the money went — and an id that is not
-- a settlement audit row, since those record internal moves and are deliberately uncounted.
UPDATE ledger
   SET id = 'rail_' || external_key,
       from_user_id = CASE WHEN (SELECT r.kind FROM rail_transfers r
                                  WHERE r.id = substr(ledger.external_key, 1, length(ledger.external_key) - 5))
                                = 'settlement' THEN to_user_id END,
       to_user_id = CASE WHEN (SELECT r.kind FROM rail_transfers r
                                WHERE r.id = substr(ledger.external_key, 1, length(ledger.external_key) - 5))
                              = 'settlement' THEN NULL ELSE to_user_id END
 WHERE external_key LIKE '%.cash';
