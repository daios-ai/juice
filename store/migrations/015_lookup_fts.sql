-- Lexical lookup index (§9): FTS5 over action name + description (+ schema field text, folded in
-- on the next activate/update). Fused with the cosine leg by reciprocal-rank fusion in Lookup, so
-- lookup works even with no embedder configured. action_id is UNINDEXED — a join/filter key only.
-- A stale row is harmless: queries re-check active/deleted against `actions`. Kept current by
-- UpsertLookupText at the same sites that store embeddings.
CREATE VIRTUAL TABLE actions_fts USING fts5(action_id UNINDEXED, text);

-- Backfill existing actions so an upgraded kernel is searchable immediately.
INSERT INTO actions_fts(action_id, text)
    SELECT id, name || ' ' || COALESCE(description, '') FROM actions WHERE deleted_at IS NULL;
