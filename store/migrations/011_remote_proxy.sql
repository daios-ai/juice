-- Add remote_proxy action kind and remote_action_id column.
-- The actions table is recreated to update the kind CHECK constraint;
-- foreign key enforcement is disabled by the migration runner.

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
    deleted_at       TEXT,
    UNIQUE (owner_user_id, name)
);

INSERT INTO actions_new
    SELECT id,owner_user_id,name,kind,active,public,price,description,
           input_schema,output_schema,source,artifact_hash,embed_vec,'',created_at,updated_at,deleted_at
    FROM actions;

DROP TABLE actions;

ALTER TABLE actions_new RENAME TO actions;
