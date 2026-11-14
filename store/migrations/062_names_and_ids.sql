-- A discovered action names its owner by principal, so the discovery cache keeps the owner's stable
-- id from the signed manifest beside the handle it already keeps (D15, D16). Rows learned before
-- this carry no id, and a handle never stands in for one, so they are dropped with their search
-- entries: the cache is regenerable (§13), and every scan restarts so the next pass refills it.
ALTER TABLE discovery_docs ADD COLUMN owner_id TEXT NOT NULL DEFAULT '';
DELETE FROM discovery_fts WHERE doc_key IN (SELECT kernel_public_key || '/' || action_id FROM discovery_docs);
DELETE FROM discovery_docs;
UPDATE kernels SET catalog_cursor = '';
