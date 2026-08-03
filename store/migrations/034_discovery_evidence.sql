-- v0.13: two regenerable caches feeding discovery search and reputation display (§13).
-- Both are truncatable with zero semantic effect (a lookup selection still resolves and
-- verifies from the home kernel; evidence is rebuilt from fresh gossip pulls) and carry no
-- foreign keys, so a purged peer's rows drop cleanly.

-- discovery_docs: searchable first-party user/action summaries learned from gossip. No price
-- column — the local marked-up price is computed through the resolve-and-cache path when the
-- action is actually considered or used, not stored here.
CREATE TABLE discovery_docs (
    kernel_public_key TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('user','action')),
    user_id           TEXT NOT NULL DEFAULT '',
    handle            TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    action_id         TEXT NOT NULL DEFAULT '',
    name              TEXT NOT NULL DEFAULT '',
    input_schema      TEXT NOT NULL DEFAULT '{}',
    output_schema     TEXT NOT NULL DEFAULT '{}',
    embed_vec         TEXT,
    observed_at       TEXT NOT NULL,
    PRIMARY KEY (kernel_public_key, kind, user_id, action_id)
);

-- FTS mirror for the lexical discovery leg; doc_key = "<kernel_public_key>/<kind>/<user_id>/<action_id>".
CREATE VIRTUAL TABLE discovery_fts USING fts5(doc_key UNINDEXED, text);

-- evidence: verified signed evidence about a subject action, keyed by (issuer, receipt_hash).
-- issuer is the gossiping kernel; subject is the executed action's stable identity. equivocated
-- marks two different valid ratings under one key (both excluded from derived metrics).
CREATE TABLE evidence (
    issuer_public_key             TEXT NOT NULL,
    receipt_hash                  TEXT NOT NULL,
    subject_kernel_public_key     TEXT NOT NULL,
    subject_action_id             TEXT NOT NULL,
    counterparty_kernel_public_key TEXT NOT NULL DEFAULT '',
    evidence_receipt_json         TEXT NOT NULL,
    rating_json                   TEXT NOT NULL DEFAULT '',
    remote_receipt_hash           TEXT NOT NULL DEFAULT '',
    receipt_created_at            TEXT NOT NULL,
    effective_at                  TEXT NOT NULL,
    observed_at                   TEXT NOT NULL,
    equivocated                   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (issuer_public_key, receipt_hash)
);

-- inspect-by-subject: derive a subject kernel's retained metrics grouped by issuer.
CREATE INDEX idx_evidence_subject ON evidence(subject_kernel_public_key, subject_action_id, receipt_created_at DESC);
-- cap partition: E most recent per (issuer, subject_kernel, subject_action).
CREATE INDEX idx_evidence_cap ON evidence(issuer_public_key, subject_kernel_public_key, subject_action_id, receipt_created_at DESC);
