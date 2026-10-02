-- A task is delivered to the party it is addressed to, on whichever kernel that party lives (U41, P8).
--
-- Every change of a task bumps its revision; a task addressed to a peer is told until the peer
-- acknowledges the revision sent. A task is delivered at revision 1 by the commit that creates it;
-- tasks made before this are delivered at startup.
ALTER TABLE tasks ADD COLUMN revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN told_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN told_failed_at TEXT;

-- The mailbox is the one record every task read goes through: this kernel's own tasks, written in
-- the commit that changes them, and tasks held by peers, written from their notices.
CREATE TABLE mailbox (
    holder_key                TEXT NOT NULL,
    id                        TEXT NOT NULL,
    required_caller_user_id   TEXT NOT NULL REFERENCES accounts(id),
    required_caller_remote_id TEXT,
    required_caller_handle    TEXT NOT NULL DEFAULT '',
    owner_user_id             TEXT REFERENCES accounts(id),
    process_id                TEXT,
    status                    TEXT NOT NULL CHECK (status IN ('waiting','running','done','cancelled')),
    revision                  INTEGER NOT NULL,
    notice_json               TEXT NOT NULL,
    created_at                TEXT NOT NULL,
    PRIMARY KEY (holder_key, id)
);
CREATE UNIQUE INDEX idx_mailbox_id ON mailbox(id);
CREATE INDEX idx_mailbox_required_caller ON mailbox(required_caller_user_id);
CREATE INDEX idx_mailbox_owner ON mailbox(owner_user_id);
