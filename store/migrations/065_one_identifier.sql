-- A kernel is named by its address and nothing else (P1): the columns that held a key now hold
-- the address, and the peer's separate payment address is the identifier itself.
ALTER TABLE kernels RENAME COLUMN public_key TO address;
ALTER TABLE kernels DROP COLUMN blockchain_address;
ALTER TABLE evidence RENAME COLUMN issuer_public_key TO issuer_kernel;
ALTER TABLE evidence RENAME COLUMN subject_kernel_public_key TO subject_kernel;
ALTER TABLE evidence RENAME COLUMN counterparty_kernel_public_key TO counterparty_kernel;
ALTER TABLE discovery_docs RENAME COLUMN kernel_public_key TO kernel;
ALTER TABLE transactions RENAME COLUMN remote_signer_key TO remote_signer;
ALTER TABLE mailbox RENAME COLUMN holder_key TO holder_kernel;
