-- Every settlement asks whether the call has a child still unsettled, and the sweep after it asks
-- which calls hold a recorded outcome (D3). Without these indexes both questions read every trace
-- ever made, so each call grew slower as the books grew. An outcome is held only until its call
-- settles (D4), so the partial index holds the waiting ones alone; outcomes left on calls settled
-- before settlement cleared them are cleared here.
UPDATE traces SET outcome_json = NULL
 WHERE outcome_json IS NOT NULL AND EXISTS (SELECT 1 FROM transactions x WHERE x.trace_id = traces.id);
CREATE INDEX idx_traces_parent ON traces(parent_trace_id);
CREATE INDEX idx_traces_outcome ON traces(id) WHERE outcome_json IS NOT NULL;
