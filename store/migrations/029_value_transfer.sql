-- v0.12 federated value transfer (§9, §13): sys/transfer carries a delivered amount through the same
-- admission / receipt / settlement pipeline as a priced call.
-- NOTE: statement comments must be on their own lines (the migration splitter carries an inline
-- trailing comment into the next statement and drops it), so every note here is a full-line comment.
--
-- traces.value / value_to: on an inbound serving trace, the amount delivered to the local beneficiary
-- and that beneficiary's user id, snapshotted at admission so settlement credits it (mirrors the
-- premium_parked snapshot). NULL on every call that is not a value transfer.
ALTER TABLE traces ADD COLUMN value INTEGER;
ALTER TABLE traces ADD COLUMN value_to TEXT;
-- receipts.value: the amount delivered to the beneficiary, distinct from charge (execution consumed).
-- value is all-or-nothing (0 or the requested amount). 0 on every non-transfer receipt.
ALTER TABLE receipts ADD COLUMN value INTEGER NOT NULL DEFAULT 0;
