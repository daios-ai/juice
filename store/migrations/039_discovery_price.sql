-- Discovery docs carry the peer's signed serving price, mp + ceil(mp·remote_bps/10000) (§13), so a
-- catalog hit can be priced without an authoritative resolve. The local all-in price adds import_bps
-- at read time, keeping the catalog repriced by local policy with no re-pull. This supersedes 034's
-- "no price column" note.
ALTER TABLE discovery_docs ADD COLUMN serving_price INTEGER NOT NULL DEFAULT 0
    CHECK (serving_price >= 0);

-- Clear the cache rather than backfill: 0 is a legal price, so a defaulted row would advertise a
-- paid action as free. The cache is regenerable and truncatable with zero effect (§3, §13) and
-- repopulates on the next discovery pass; briefly omitting a stale row is honest, mispricing it is not.
DELETE FROM discovery_fts;
DELETE FROM discovery_docs;
