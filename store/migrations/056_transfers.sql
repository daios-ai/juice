-- A transfer reaches a user of any kernel (D18, P11).
--
-- A run may carry its caller's key, so a retried run is answered with the first one's outcome
-- rather than run again (D20); the key is the run's, so it lives on the process the run created.
ALTER TABLE processes ADD COLUMN external_key TEXT;
ALTER TABLE processes ADD COLUMN request_hash TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_processes_external_key ON processes(owner_user_id, external_key)
    WHERE external_key IS NOT NULL;

-- The beneficiary's kernel, empty for one here.
ALTER TABLE traces ADD COLUMN value_peer TEXT NOT NULL DEFAULT '';

-- A forgotten kernel that proved a vault keeps its row, key and vault only: a payment from the vault
-- may arrive after the kernel is forgotten, and must still be recognised as its (D23).
ALTER TABLE kernels ADD COLUMN forgotten_at TEXT;

-- A peer's word that a payment of its is for one of our users (P11).
CREATE TABLE incoming_transfers (
    counterparty   TEXT NOT NULL,
    id             TEXT NOT NULL,
    beneficiary_id TEXT NOT NULL REFERENCES accounts(id),
    amount         INTEGER NOT NULL CHECK (amount > 0),
    payer          TEXT NOT NULL DEFAULT '',
    tx_hash        TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('announced', 'credited')),
    created_at     TEXT NOT NULL,
    PRIMARY KEY (counterparty, id)
);

-- A transfer's payment to the beneficiary's kernel is a rail row of its own kind (P11).
ALTER TABLE rail_transfers RENAME TO rail_transfers_old;
CREATE TABLE rail_transfers (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('deposit','payout','obligation','settlement','claim','refill','transfer')),
    party        TEXT NOT NULL DEFAULT '',
    amount       INTEGER NOT NULL CHECK (amount > 0),
    credit       INTEGER NOT NULL DEFAULT 0 CHECK (credit >= 0 AND credit <= amount),
    destination  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('drawing','pending','submitted','refilling','blocked','confirmed','failed','announced','held','credited')),
    tx_hash      TEXT NOT NULL DEFAULT '',
    refill_id    TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    attempt      INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    created_at   TEXT NOT NULL,
    finalized_at TEXT
);
INSERT INTO rail_transfers (id,kind,party,amount,credit,destination,status,tx_hash,refill_id,reason,attempt,created_at,finalized_at)
SELECT id,kind,party,amount,credit,destination,status,tx_hash,refill_id,reason,attempt,created_at,finalized_at FROM rail_transfers_old;
DROP TABLE rail_transfers_old;
CREATE INDEX idx_rail_transfers_open ON rail_transfers(kind, status);
CREATE INDEX idx_rail_transfers_party ON rail_transfers(party);
