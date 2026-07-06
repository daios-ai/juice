-- Delegated upstream OAuth grants (§8): a user's consent for one specific action to
-- wield their upstream identity. The refresh_token is AES-256-GCM sealed at rest and
-- write-only; one row per (grantor, action), re-consent overwrites.
CREATE TABLE grants (
    id              TEXT PRIMARY KEY,
    grantor_user_id TEXT NOT NULL REFERENCES users(id),
    action_id       TEXT NOT NULL REFERENCES actions(id),
    refresh_token   TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    UNIQUE (grantor_user_id, action_id)
);
CREATE INDEX idx_grants_action ON grants(action_id);
