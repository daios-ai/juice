ALTER TABLE action_stats RENAME COLUMN price_mean TO cost_estimate;
ALTER TABLE action_stats RENAME COLUMN latency_mean TO latency_estimate;
ALTER TABLE action_stats RENAME COLUMN rating_mean TO rating_estimate;
