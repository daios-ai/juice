-- The value channel is local and untaxed, so nothing levies a premium on it: every issuer wrote 0,
-- and verification compared 0 against 0. The column, its struct field, and its check go together
-- rather than surviving as a constant nobody can set.
--
-- No signed record is invalidated. `value_premium` carries `omitempty`, so a 0 was never part of any
-- receipt's canonical signing payload; dropping it leaves every stored signature verifying byte for
-- byte. The guard covers the only receipts that could disagree — those written by a build that still
-- priced value across a kernel boundary — and aborts the migration rather than silently discarding
-- the one field their signatures cover.

CREATE TABLE migration_044_guard (
    signed_premium INTEGER NOT NULL
        CONSTRAINT "receipts carry a nonzero value_premium: their signatures cover it, so export them before upgrading"
        CHECK (signed_premium = 0)
);

INSERT INTO migration_044_guard (signed_premium)
VALUES ((SELECT COUNT(*) FROM receipts WHERE COALESCE(value_premium, 0) <> 0));

DROP TABLE migration_044_guard;

ALTER TABLE receipts DROP COLUMN value_premium;
