-- Soft-delete support for actions.
-- Physical DELETE is replaced by setting deleted_at; transaction history is preserved.
ALTER TABLE actions ADD COLUMN deleted_at TEXT;
