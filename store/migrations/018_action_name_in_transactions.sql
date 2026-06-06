-- Capture action name at call time so transactions are self-contained after the action is deleted.
ALTER TABLE transactions ADD COLUMN action_name TEXT NOT NULL DEFAULT '';
