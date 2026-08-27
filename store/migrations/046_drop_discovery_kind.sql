-- The discovery cache carried two doc kinds; the user kind is gone (users are discovered as
-- owners of public actions), so the discriminator says nothing and the owner principal id rode
-- only on user docs' behalf. One doc per remote action, keyed by the serving kernel and the
-- action's stable id (per-kernel unique by manifest construction). The cache is regenerable
-- (§13): drop and recreate empty; the next gossip pass repopulates it.

DROP TABLE discovery_docs;
CREATE TABLE discovery_docs (
    kernel_public_key TEXT NOT NULL,
    handle            TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    action_id         TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    input_schema      TEXT NOT NULL DEFAULT '{}',
    output_schema     TEXT NOT NULL DEFAULT '{}',
    embed_vec         TEXT,
    observed_at       TEXT NOT NULL,
    serving_price     INTEGER NOT NULL DEFAULT 0 CHECK (serving_price >= 0),
    PRIMARY KEY (kernel_public_key, action_id)
);

DROP TABLE discovery_fts;
CREATE VIRTUAL TABLE discovery_fts USING fts5(doc_key UNINDEXED, text);
