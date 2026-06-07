-- Restore soft-delete for actions: disable discovery, preserve history.
ALTER TABLE actions ADD COLUMN deleted_at TEXT;
