-- v0.13: a rating carries the SHA-256 of its rated receipt's canonical JSON, the portable
-- link a gossip evidence bundle uses to join a rating to its receipt (§13). Empty on
-- pre-v0.13 ratings, which stay local-only (never gossip-eligible) and whose immutable
-- signatures are unaffected (the field is omitempty, so it is absent from their canonical payload).
ALTER TABLE ratings ADD COLUMN rated_receipt_hash TEXT NOT NULL DEFAULT '';
