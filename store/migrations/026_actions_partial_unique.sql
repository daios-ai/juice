-- Replace full UNIQUE (owner_user_id, name) with a partial index that excludes soft-deleted rows,
-- allowing the same name to be reused after an action is soft-deleted.

CREATE TABLE actions_new (
    id               TEXT PRIMARY KEY,
    owner_user_id    TEXT NOT NULL REFERENCES users(id),
    name             TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('http','wasm','native','remote_proxy')),
    active           INTEGER NOT NULL DEFAULT 0,
    public           INTEGER NOT NULL DEFAULT 0,
    price            INTEGER NOT NULL DEFAULT 0 CHECK (price >= 0),
    description      TEXT NOT NULL DEFAULT '',
    input_schema     TEXT NOT NULL DEFAULT '{}',
    output_schema    TEXT NOT NULL DEFAULT '{}',
    source           TEXT NOT NULL DEFAULT '',
    artifact_hash    TEXT NOT NULL DEFAULT '',
    embed_vec        TEXT,
    remote_action_id TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    deleted_at       TEXT
);

INSERT INTO actions_new
    SELECT id,owner_user_id,name,kind,active,public,price,description,
           input_schema,output_schema,source,artifact_hash,embed_vec,remote_action_id,
           created_at,updated_at,deleted_at
    FROM actions;

CREATE TABLE listeners_new (
    id               TEXT PRIMARY KEY,
    owner_user_id    TEXT NOT NULL REFERENCES users(id),
    source_user_id   TEXT NOT NULL REFERENCES users(id),
    event_name       TEXT NOT NULL,
    target_action_id TEXT NOT NULL REFERENCES actions_new(id) ON DELETE CASCADE,
    active           INTEGER NOT NULL DEFAULT 1,
    created_at       TEXT NOT NULL
);

INSERT INTO listeners_new SELECT * FROM listeners;

DROP TABLE listeners;
DROP TABLE actions;

ALTER TABLE actions_new RENAME TO actions;
ALTER TABLE listeners_new RENAME TO listeners;

CREATE UNIQUE INDEX idx_actions_owner_name_active ON actions(owner_user_id, name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_actions_owner ON actions(owner_user_id);
CREATE INDEX IF NOT EXISTS idx_listeners_source_event ON listeners(source_user_id, event_name);
