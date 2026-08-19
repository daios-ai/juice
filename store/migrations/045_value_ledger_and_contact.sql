-- Two display-and-audit gaps close together.
--
-- A delivered transfer value moved balances with no entry in the journal that records every other
-- balance movement, and the beneficiary is no party to the settling transaction — so credit arrived
-- with nothing to read. Settlement now writes one ledger entry per delivery; the backfill gives past
-- deliveries the same row, reconstructed from the immutable receipt that signed each one. The id
-- derives from the transaction, exactly as the live write derives it, so a re-run inserts nothing new
-- and a delivery can never be journalled twice.
--
-- Both parties must still exist locally: the ledger's foreign keys name `accounts`, and a receipt
-- written when value could cross a kernel boundary may name a beneficiary that was never an account
-- here. Such a row is skipped rather than aborting the migration — its money moved on a contract this
-- kernel no longer speaks, and the receipt remains its record.

INSERT INTO ledger (id, operator_user_id, from_user_id, to_user_id, amount, reason, external_key, created_at)
SELECT 'tv_' || r.tx_id, r.caller_user_id, r.caller_user_id, r.value_to, r.value, r.tx_id, NULL, r.created_at
FROM receipts r
WHERE r.status = 'success'
  AND COALESCE(r.value, 0) > 0
  AND r.value_to IS NOT NULL
  AND EXISTS (SELECT 1 FROM accounts a WHERE a.id = r.caller_user_id)
  AND EXISTS (SELECT 1 FROM accounts a WHERE a.id = r.value_to)
  AND NOT EXISTS (SELECT 1 FROM ledger l WHERE l.id = 'tv_' || r.tx_id);

-- Reachability is not retention. A kernel row is kept for 90 idle days; whether the peer answered
-- just now is a different question, and the catalog answered neither — a call could fail and the next
-- search would still present that kernel's actions as if nothing had happened. Two timestamps carry
-- it: `last_seen` (already here) and its twin below. Each only ever moves forward and neither is
-- cleared, so concurrent observations cannot overwrite newer truth; a consumer compares the two and
-- applies its own freshness policy. Display cache only — nothing gates on them.

ALTER TABLE kernels ADD COLUMN last_contact_failed_at TEXT;
