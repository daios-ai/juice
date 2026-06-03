-- Add caller identity, process context, and call start time to receipts
-- so action owners can audit revenue without joining to transactions.

ALTER TABLE receipts ADD COLUMN caller_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE receipts ADD COLUMN process_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE receipts ADD COLUMN started_at     TEXT NOT NULL DEFAULT '';

UPDATE receipts
SET caller_user_id = (SELECT subject_user_id FROM transactions WHERE transactions.id = receipts.tx_id),
    process_id     = (SELECT process_id     FROM transactions WHERE transactions.id = receipts.tx_id),
    started_at     = (SELECT started_at     FROM transactions WHERE transactions.id = receipts.tx_id);

CREATE INDEX IF NOT EXISTS idx_receipts_action_started ON receipts(action_id, started_at DESC);
