-- A remote_proxy row is kernel-managed and always visibility=local (§8): callable by this kernel's
-- own users, never re-served to a further peer. Rows imported before that rule was enforced are
-- public, so they surface in anonymous GET /v1/actions. One-time data correction; ids, stats, and
-- history are untouched. Rows created but never promoted stay private and inactive.
UPDATE actions SET visibility = 'local' WHERE kind = 'remote_proxy' AND visibility = 'public';
