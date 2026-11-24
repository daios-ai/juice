-- A kernel is paid at its own key's address (P1): the address is computed from the key, so the
-- proof that used to bind a separate rail key to it has nothing left to prove.
ALTER TABLE kernels DROP COLUMN blockchain_proof;
