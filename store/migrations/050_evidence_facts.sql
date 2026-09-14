-- Evidence is stored with what verifies it and what joins it (G7, U36, P9).
-- A receipt carries its own canonical hash, written with it, so a peer's rating joins it by a
-- query rather than by hashing every receipt on read. A settled remote receipt carries the key
-- it verified under, so verification never depends on a peer account retention may empty.
ALTER TABLE receipts ADD COLUMN hash TEXT;
CREATE INDEX IF NOT EXISTS receipts_hash ON receipts(hash);
ALTER TABLE transactions ADD COLUMN remote_signer_key TEXT;
-- Existing remote transactions take the key from the peer that still holds it. Those whose peer
-- was purged before this record existed stay unkeyed (§10). Receipt hashes are a canonical
-- serialization no query can compute; the store fills them once at the next open.
UPDATE transactions SET remote_signer_key = (SELECT kernel_public_key FROM accounts WHERE accounts.id = transactions.target_user_id)
 WHERE remote_receipt_json <> '' AND remote_signer_key IS NULL;
-- A call settles exactly once (G1): the schema enforces it, not the paths that race for it.
DROP INDEX IF EXISTS idx_transactions_trace;
CREATE UNIQUE INDEX IF NOT EXISTS idx_transactions_trace ON transactions(trace_id);
-- A trace settles only after every trace beneath it (D3). A call whose execution has ended while a
-- child is still in flight records its outcome here and is settled by the last child's settlement,
-- so its receipt signs a charge that is final.
ALTER TABLE traces ADD COLUMN outcome_json TEXT;
