ALTER TABLE idempotency_records ADD COLUMN status TEXT NOT NULL DEFAULT 'complete';
ALTER TABLE idempotency_records ADD COLUMN result_json TEXT NOT NULL DEFAULT '';
