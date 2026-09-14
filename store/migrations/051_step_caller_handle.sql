-- A step parked for a principal on a peer kept only that principal's stable id, so it could be
-- named to an operator by nothing friendlier. The handle the resolve already returned is kept
-- beside it, display only: the id above stays the identity a completion is authorised against, so
-- a rename on the peer leaves the step correctly addressed and only this column goes stale.
-- Rows written before this keep an empty handle and are rendered by their id, as they were.
ALTER TABLE steps ADD COLUMN required_caller_handle TEXT NOT NULL DEFAULT '';
