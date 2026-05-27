-- Listeners no longer store process or trace; consume supplies the process at runtime.
ALTER TABLE listeners DROP COLUMN process_id;
ALTER TABLE listeners DROP COLUMN trace_id;
