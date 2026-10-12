-- A task acts for its requester, never for the party who completes it, and a party decides who may
-- address tasks to them (D6, U52). The requester is recorded at creation; a task's transfer value is
-- reserved from it then, beside the price.
--
-- An outstanding task — waiting, or running with no transaction, which startup turns back to
-- waiting — would complete under the old law: its value was never reserved, and who requested it
-- cannot be told after the fact. None may exist.
CREATE TABLE migration_060_guard (
    outstanding_tasks INTEGER NOT NULL
        CONSTRAINT "a task is still waiting or running: complete or decline every task before upgrading"
        CHECK (outstanding_tasks = 0)
);
INSERT INTO migration_060_guard (outstanding_tasks)
SELECT COUNT(*) FROM tasks WHERE status = 'waiting' OR (status = 'running' AND tx_id IS NULL);
DROP TABLE migration_060_guard;

ALTER TABLE tasks ADD COLUMN requester_user_id TEXT REFERENCES accounts(id);
ALTER TABLE tasks ADD COLUMN value INTEGER NOT NULL DEFAULT 0 CHECK (value >= 0);
ALTER TABLE tasks ADD COLUMN value_to TEXT;
ALTER TABLE tasks ADD COLUMN value_peer TEXT NOT NULL DEFAULT '';

ALTER TABLE mailbox ADD COLUMN requester_kernel TEXT NOT NULL DEFAULT '';
ALTER TABLE mailbox ADD COLUMN requester_user_id TEXT NOT NULL DEFAULT '';
ALTER TABLE mailbox ADD COLUMN requester_handle TEXT NOT NULL DEFAULT '';

-- Who may address tasks to a user of this kernel: pending from first contact until the user
-- approves or blocks. A row is never deleted, so a revoked requester stays listed as pending.
CREATE TABLE requesters (
    user_id           TEXT NOT NULL REFERENCES accounts(id),
    requester_kernel  TEXT NOT NULL,
    requester_user_id TEXT NOT NULL,
    requester_handle  TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL CHECK (status IN ('pending','approved','blocked')),
    created_at        TEXT NOT NULL,
    PRIMARY KEY (user_id, requester_kernel, requester_user_id)
);
