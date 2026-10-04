-- An action carries a title: the short name a person reads in a catalogue, beside the address that
-- names it for software (D4). It is a contract field — quoted, signed into the manifest, hashed.
--
-- Existing rows take the last segment of their name verbatim, cut to the 80-character limit, since
-- no one is present to write one; their owner edits it with `action update --title`. rtrim strips
-- the trailing run of characters that are not '/', which leaves the path up to the last '/'; the
-- replace removes that prefix (a name without '/' keeps itself, replace of '' being the identity).
ALTER TABLE actions ADD COLUMN title TEXT NOT NULL DEFAULT '';
UPDATE actions SET title = substr(replace(name, rtrim(name, replace(name, '/', '')), ''), 1, 80);

-- Discovered actions are a regenerable cache: the same rule until the next pull replaces it.
ALTER TABLE discovery_docs ADD COLUMN title TEXT NOT NULL DEFAULT '';
UPDATE discovery_docs SET title = substr(replace(name, rtrim(name, replace(name, '/', '')), ''), 1, 80);

-- A cached remote action is re-resolved on its next call, so it takes the title its provider wrote
-- and the contract hash that now covers it (D13).
UPDATE actions SET active = 0 WHERE kind = 'remote_proxy';
