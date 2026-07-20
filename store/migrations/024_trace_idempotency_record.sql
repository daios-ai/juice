-- The inbound cross-kernel idempotency record a trace is executing for (§13), when the call is
-- serving a peer. Persisting it on the trace — rather than inside dispatch_json, which exists only
-- for remote-proxy traces (§3) — means the settlement that finally resolves ANY parked or crashed
-- federated call completes that record: retry, max-age bound, forced closure, or crash recovery.
-- Without it a peer is answered "duplicate in flight" until the record expires and can never learn
-- the outcome of work it paid for. Null for locally-originated calls and for subcalls.
ALTER TABLE traces ADD COLUMN idempotency_record_id TEXT;
