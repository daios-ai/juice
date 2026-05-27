-- Add FOLLOWS_FROM causal trace link for event-triggered calls.
ALTER TABLE traces ADD COLUMN caused_by_trace_id TEXT;
