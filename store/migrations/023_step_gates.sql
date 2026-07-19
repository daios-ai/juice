-- Counter state for the @sys/step/join native (§9). A barrier is inherently shared mutable state:
-- one row per onward step, deleted when the join resolves. Native-owned — the kernel never reads it,
-- and the join handler reaches it through an injected interface, not through kernel.Store.
-- ON DELETE CASCADE: a cancelled or purged step takes its gate with it, so an abandoned barrier
-- (e.g. a force-closed process) leaves no orphan row behind.
CREATE TABLE step_gates (
    step_id    TEXT PRIMARY KEY REFERENCES steps(id) ON DELETE CASCADE,
    need       INTEGER NOT NULL CHECK (need >= 1),
    have       INTEGER NOT NULL DEFAULT 0 CHECK (have >= 0),
    updated_at TEXT NOT NULL
);
