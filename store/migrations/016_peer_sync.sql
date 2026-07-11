-- §13 peer sync cache: a successful friend gossip pull persists liveness and our credit on the
-- peer. Display-only (§14); null on non-peer rows and never execution semantics.
ALTER TABLE users ADD COLUMN peer_last_seen TEXT;
ALTER TABLE users ADD COLUMN peer_credit INTEGER;
