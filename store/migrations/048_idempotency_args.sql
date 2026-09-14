-- An inbound call's arguments live on its idempotency record. A provider killed mid-call settles
-- the trace at restart as an interrupted failure and signs a receipt over the arguments it was
-- given; without them the receipt hashes an empty payload, the caller rejects it, and the caller's
-- funds stay locked until the pending-call bound expires (G4, D3).
ALTER TABLE idempotency_records ADD COLUMN args_json TEXT NOT NULL DEFAULT '';
