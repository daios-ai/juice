-- v0.13: PEX peer-exchange discovery (§13). A discovered kernel carries pull-attempt state so
-- the discovery loop rotates candidates least-recently-attempted first and evicts a poisoned
-- stub after a bounded number of non-verified pulls. last_attempt_at is the last pull attempt
-- (success or failure); attempts is consecutive non-verified pulls, reset to 0 on a verified pull.
-- Both regenerable — a re-pull re-derives them; existing rows default to never-attempted.
ALTER TABLE discovered_kernels ADD COLUMN last_attempt_at TEXT NOT NULL DEFAULT '';
ALTER TABLE discovered_kernels ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
