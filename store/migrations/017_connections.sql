-- Connections (§8): a user's upstream account credential, stored once per (user, provider_key)
-- and shared by every Grant that points at it. sealed_secret is AES-256-GCM sealed with AAD
-- user_id|connection_id and write-only. This splits the credential (Connection) from the
-- per-action consent (Grant): the grant loses its own token and gains connection_id.
-- refresh_token on grants is retained, now nullable, only for the one-time backfill that
-- re-homes each legacy per-grant token onto its derived connection; it is cleared on link.
CREATE TABLE connections (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id),
    provider_key  TEXT NOT NULL,
    sealed_secret TEXT NOT NULL,
    scopes_json   TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (user_id, provider_key)
);

-- Rebuild grants: add connection_id, relax refresh_token to nullable (backfill-only).
CREATE TABLE grants_new (
    id              TEXT PRIMARY KEY,
    grantor_user_id TEXT NOT NULL REFERENCES users(id),
    action_id       TEXT NOT NULL REFERENCES actions(id),
    connection_id   TEXT REFERENCES connections(id),
    refresh_token   TEXT,
    created_at      TEXT NOT NULL,
    UNIQUE (grantor_user_id, action_id)
);
INSERT INTO grants_new (id,grantor_user_id,action_id,connection_id,refresh_token,created_at)
    SELECT id,grantor_user_id,action_id,NULL,refresh_token,created_at FROM grants;
DROP TABLE grants;
ALTER TABLE grants_new RENAME TO grants;
CREATE INDEX idx_grants_action ON grants(action_id);
CREATE INDEX idx_grants_connection ON grants(connection_id);
