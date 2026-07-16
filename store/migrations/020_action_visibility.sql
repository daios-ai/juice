-- §3/§4: three-valued visibility replaces the boolean public flag.
-- private = owner-only; local = any local (non-peer) caller, never served to peers;
-- public = callable by anyone and served in manifests/gossip.
-- Mapping: public=1 -> 'public', public=0 -> 'private' (the new default); 'local' is new.
-- `public` is referenced by no index/trigger/view, so plain DROP COLUMN is safe (same as 013).
ALTER TABLE actions ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private'
    CHECK (visibility IN ('private','local','public'));
UPDATE actions SET visibility = CASE WHEN public = 1 THEN 'public' ELSE 'private' END;
ALTER TABLE actions DROP COLUMN public;
