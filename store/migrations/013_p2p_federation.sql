-- v0.5: federation moves to a libp2p transport addressed by Ed25519 public key.
-- Location is no longer stored anywhere; the transport resolves a peer key to a live
-- path at call time. A user's kind is now determined by public_key alone (null =
-- local, set = proxy), and discovered kernels carry no URL. Drop the two URL columns.
-- Neither column participates in a primary key, index, CHECK, or foreign key, so a
-- plain DROP COLUMN is safe.

ALTER TABLE users DROP COLUMN remote_base_url;

ALTER TABLE discovered_kernels DROP COLUMN base_url;
