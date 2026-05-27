-- Rating mean uses rated observations as the denominator, not action uses.
ALTER TABLE action_stats ADD COLUMN rating_count INTEGER NOT NULL DEFAULT 0;
