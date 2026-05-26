-- Add suspended_at to users
ALTER TABLE users ADD COLUMN suspended_at TEXT;

-- Add public (grant-all) flag to actions
ALTER TABLE actions ADD COLUMN public INTEGER NOT NULL DEFAULT 0;

-- Add cost and latency_ms to traces
ALTER TABLE traces ADD COLUMN cost INTEGER NOT NULL DEFAULT 0;
ALTER TABLE traces ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT 0;

-- Config table for persistent key/value (superuser handle etc.)
CREATE TABLE IF NOT EXISTS config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
