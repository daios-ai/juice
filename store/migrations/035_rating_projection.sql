-- v0.13: gossip now carries a privacy-preserving rating projection {rating, note, created_at,
-- rated_receipt_hash, signature} rather than the full signed Rating (§13). The evidence cache is
-- regenerable, so drop it and reset every peer's gossip cursor; peers re-pull from scratch and
-- store projections. Without this, a cached full-Rating row would false-flag equivocation against
-- the re-gossiped projection for the same (issuer, receipt_hash).
DELETE FROM evidence;
UPDATE discovered_kernels SET gossip_cursor = '';
