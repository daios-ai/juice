-- v0.13: gossip reputation moves from unverifiable third-party aggregates to signed,
-- independently verifiable evidence (§13). The stat_tags table held introducer-namespaced
-- gossip aggregates fed into a ranking prior that is already removed (ranking is relevance-only).
-- No production reader remains; drop it.
DROP TABLE IF EXISTS stat_tags;
