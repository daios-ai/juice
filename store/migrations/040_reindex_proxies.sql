-- A resolved remote_proxy activates inside ImportPeerAction rather than through SetActive, so until
-- now it was never written to the lexical index (§9). Because an active proxy also shadows the
-- discovery row it replaces, resolving an action removed it from search entirely. The code path is
-- fixed; this repairs rows already created by it.
--
-- Repair the derived data directly rather than deactivating the rows: deactivation would mutate
-- valid domain state to trigger a network repair, and calls by raw action id do not re-resolve
-- (only the kernel-qualified branch does), so those would break until someone called by reference.
--
-- Text matches migration 015's backfill precedent — name + description, with schema text folded in
-- on the next activate/update. Embeddings cannot be generated in SQL; the lexical leg alone
-- restores findability (§9: lookup degrades to lexical with no embedder configured).
INSERT INTO actions_fts(action_id, text)
    SELECT a.id, a.name || ' ' || COALESCE(a.description, '')
    FROM actions a
    WHERE a.kind = 'remote_proxy'
      AND a.active = 1
      AND a.deleted_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM actions_fts f WHERE f.action_id = a.id);
