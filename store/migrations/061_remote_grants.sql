-- A grant follows its grantor's process across kernels (D10). A grantor and a connection's user are
-- principals — the kernel they live on, empty for this one, and the user there — so neither table
-- references an account any longer. Nothing sealed is rewritten: a local user's AAD is unchanged.
CREATE TABLE connections_061 (
    id            TEXT PRIMARY KEY,
    user_kernel   TEXT NOT NULL DEFAULT '',
    user_id       TEXT NOT NULL,
    provider_key  TEXT NOT NULL,
    sealed_secret TEXT NOT NULL,
    scopes_json   TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (user_kernel, user_id, provider_key)
);
INSERT INTO connections_061 (id, user_id, provider_key, sealed_secret, scopes_json, created_at, updated_at)
SELECT id, user_id, provider_key, sealed_secret, scopes_json, created_at, updated_at FROM connections;

CREATE TABLE grants_061 (
    id              TEXT PRIMARY KEY,
    grantor_kernel  TEXT NOT NULL DEFAULT '',
    grantor_user_id TEXT NOT NULL,
    action_id       TEXT NOT NULL REFERENCES actions(id),
    connection_id   TEXT REFERENCES connections_061(id),
    created_at      TEXT NOT NULL,
    UNIQUE (grantor_kernel, grantor_user_id, action_id)
);
INSERT INTO grants_061 (id, grantor_user_id, action_id, connection_id, created_at)
SELECT id, grantor_user_id, action_id, connection_id, created_at FROM grants;
DROP TABLE grants;
DROP TABLE connections;
ALTER TABLE connections_061 RENAME TO connections;
ALTER TABLE grants_061 RENAME TO grants;
CREATE INDEX idx_grants_action ON grants(action_id);
CREATE INDEX idx_grants_connection ON grants(connection_id);

-- A cached remote action says whether calling it needs the caller's grant (P6).
ALTER TABLE actions ADD COLUMN requires_grant INTEGER NOT NULL DEFAULT 0;

-- Every process has a reference another kernel can ask this one about. A process serving a call
-- carries the reference it was given and the kernel that gave it; `registered` is 1 once that
-- kernel holds this process open and 2 once it has been told the process closed.
ALTER TABLE processes ADD COLUMN ref TEXT NOT NULL DEFAULT '';
ALTER TABLE processes ADD COLUMN origin_kernel TEXT NOT NULL DEFAULT '';
ALTER TABLE processes ADD COLUMN registered INTEGER NOT NULL DEFAULT 0;
UPDATE processes SET ref = lower(hex(randomblob(32))) WHERE status = 'open';
CREATE UNIQUE INDEX idx_processes_ref ON processes(ref) WHERE ref <> '' AND origin_kernel = '';

-- Work another kernel holds for a process of this one: the process stays open while a row is
-- unreleased. A released row stays until the process closes, so a late repeat cannot restore it.
CREATE TABLE registrations (
    process_id TEXT NOT NULL REFERENCES processes(id),
    kernel     TEXT NOT NULL,
    reg_id     TEXT NOT NULL,
    released   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (process_id, kernel, reg_id)
);

-- The newest grant operation applied for a user of another kernel, in nanoseconds: an older one
-- arriving late is refused.
CREATE TABLE grant_ops (
    kernel  TEXT NOT NULL,
    user_id TEXT NOT NULL,
    applied INTEGER NOT NULL,
    PRIMARY KEY (kernel, user_id)
);
