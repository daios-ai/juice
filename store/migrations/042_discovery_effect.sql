-- A discovery doc carries the manifest's `effect` so a catalog hit and the local proxy it resolves
-- to produce the same quote_hash (§4 precondition 7). Effect alone decides whether a call engages
-- the value channel (§13), so a quote that omits it would not bind whether a second, separate
-- charge is taken from the caller's own balance.
ALTER TABLE discovery_docs ADD COLUMN effect TEXT NOT NULL DEFAULT '';

-- Clear the cache rather than backfill, exactly as 039 did for serving_price: '' is a legal effect,
-- so a defaulted row would quote a transfer action as ordinary and a buyer pinning that quote would
-- be admitted to a value-bearing call it never agreed to. The cache is regenerable and truncatable
-- with zero effect (§3, §13) and repopulates on the next discovery pass.
DELETE FROM discovery_fts;
DELETE FROM discovery_docs;
