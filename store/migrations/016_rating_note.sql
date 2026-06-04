-- Add optional note to ratings for human-readable justification.
ALTER TABLE ratings ADD COLUMN note TEXT;
