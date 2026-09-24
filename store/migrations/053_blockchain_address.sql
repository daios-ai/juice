-- One name for the address money is paid to, and one for the network's identity (D4, D20, D23).
--
-- `rail_address` named the layer that moves the money rather than the thing itself: an address
-- on the chain. `network_digest` named how the identity is computed rather than what it is for:
-- a fingerprint a person compares by eye. Both are renamed in place, in every column and in the
-- one config key that pins a kernel to its network. Nothing changes meaning, so nothing is guarded.
ALTER TABLE accounts RENAME COLUMN rail_address TO blockchain_address;
DROP INDEX idx_accounts_rail_address;
CREATE UNIQUE INDEX idx_accounts_blockchain_address ON accounts(blockchain_address) WHERE blockchain_address IS NOT NULL;
ALTER TABLE kernels RENAME COLUMN rail_address TO blockchain_address;
ALTER TABLE kernels RENAME COLUMN rail_proof TO blockchain_proof;
ALTER TABLE traces RENAME COLUMN owed_rail_address TO owed_blockchain_address;
UPDATE config SET key = 'world_fingerprint' WHERE key = 'world_digest';
