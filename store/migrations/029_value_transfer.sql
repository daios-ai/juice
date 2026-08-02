-- v0.12 federated value transfer (§9, §13): sys/transfer is an ordinary priced action whose successful
-- execution commits an additional deferred transfer effect, funded by the immediate caller C, carried
-- through the same admission / receipt / settlement pipeline.
-- NOTE: statement comments must be on their own lines (the migration splitter carries an inline
-- trailing comment into the next statement and drops it), so every note here is a full-line comment.
--
-- actions.effect: a signed manifest contract field naming a privileged execution effect ("transfer").
-- The origin decides a remote action is value-bearing from this signed field, never a name coincidence.
-- NULL on every ordinary action.
ALTER TABLE actions ADD COLUMN effect TEXT;
-- traces.value / value_to / value_reserve: the TransferEffect snapshot on a call whose caller C funds a
-- transfer — the delivered amount, the resolved local beneficiary user id, and the total reserve locked
-- from C.available at admission (value + value fees). Released to the beneficiary/peer + sys + refund at
-- settlement (mirrors premium_parked). NULL/0 on every non-transfer call.
ALTER TABLE traces ADD COLUMN value INTEGER;
ALTER TABLE traces ADD COLUMN value_to TEXT;
ALTER TABLE traces ADD COLUMN value_reserve INTEGER;
-- receipts.value / value_premium / value_to: the transfer channel, kept distinct from the execution
-- channel (charge/premium). value is the delivered amount (all-or-nothing: 0 or the requested amount),
-- value_premium the serving markup on the value, value_to the resolved beneficiary the origin binds. 0/""
-- on every non-transfer receipt (JCS omitempty keeps old receipts verifying).
ALTER TABLE receipts ADD COLUMN value INTEGER NOT NULL DEFAULT 0;
ALTER TABLE receipts ADD COLUMN value_premium INTEGER NOT NULL DEFAULT 0;
ALTER TABLE receipts ADD COLUMN value_to TEXT;
