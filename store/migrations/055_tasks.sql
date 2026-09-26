-- A step is a task (D6): a funded call waiting for the one party it names. The table and its
-- indexes are renamed in place, and so is the key a trace's own records carry for the task it
-- completes, since a retry reads it back (D19). Signed and committed records — receipts,
-- transactions, their arguments and replies — are history and keep the words they were written in
-- (G3). Nothing changes meaning, so nothing is guarded.
ALTER TABLE steps RENAME TO tasks;
DROP INDEX idx_steps_required_caller;
DROP INDEX idx_steps_status;
CREATE INDEX idx_tasks_required_caller ON tasks(required_caller_user_id);
CREATE INDEX idx_tasks_status          ON tasks(status);

UPDATE traces SET dispatch_json = json_set(json_remove(dispatch_json, '$.step_id'), '$.task_id', json_extract(dispatch_json, '$.step_id'))
 WHERE json_valid(dispatch_json) AND json_type(dispatch_json, '$.step_id') IS NOT NULL;
UPDATE traces SET outcome_json = json_set(json_remove(outcome_json, '$.step_id'), '$.task_id', json_extract(outcome_json, '$.step_id'))
 WHERE json_valid(outcome_json) AND json_type(outcome_json, '$.step_id') IS NOT NULL;
