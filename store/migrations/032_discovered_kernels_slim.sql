-- v0.13: gossip is first-party only, so a discovered kernel is one row per key (not per
-- introducer) and carries no third-party stats blob (§13). Rebuild the table: drop
-- introduced_by and stats_json, key by public_key alone, add `about` (the kernel's
-- self-description carried in gossip) and `gossip_cursor` (the persisted evidence
-- high-watermark for that peer, §13 peer sync). Rows collapse to one per key, keeping the
-- earliest first_seen and latest updated_at; handle is regenerable from the next gossip pull.
CREATE TABLE discovered_kernels_new (
    public_key    TEXT PRIMARY KEY,
    handle        TEXT NOT NULL DEFAULT '',
    about         TEXT NOT NULL DEFAULT '',
    gossip_cursor TEXT NOT NULL DEFAULT '',
    first_seen    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

INSERT INTO discovered_kernels_new (public_key, handle, first_seen, updated_at)
    SELECT public_key, MIN(handle), MIN(first_seen), MAX(updated_at)
    FROM discovered_kernels
    GROUP BY public_key;

DROP TABLE discovered_kernels;
ALTER TABLE discovered_kernels_new RENAME TO discovered_kernels;
