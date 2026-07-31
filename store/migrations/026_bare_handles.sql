-- v0.12: handles are stored BARE — the `@` sigil moves out of storage into reference syntax only
-- (§3, §14). Every existing handle was normalized with a leading `@` (NormalizeHandle prepended it),
-- so stripping the prefix preserves uniqueness (a set of distinct `@x` maps to a distinct set of `x`).
-- Runs before 027 so 027's role/handle-keyed logic sees the stripped `superuser_handle`.
UPDATE users SET handle = substr(handle, 2) WHERE handle LIKE '@%';

-- Config values that embed a handle follow: superuser_handle ('@sys' -> 'sys') and
-- kernel_handle ('@k-abc123' -> 'k-abc123'). Other config values are untouched.
UPDATE config SET value = substr(value, 2)
 WHERE key IN ('superuser_handle', 'kernel_handle') AND value LIKE '@%';
