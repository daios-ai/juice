-- Drop the barrier counter table: @sys/step/race and @sys/step/join are withdrawn from the
-- platform stdlib (§9). Step coordination beyond a step's own one-shot resumption belongs in
-- user-land actions, not in kernel-adjacent state.
--
-- Existing rows are discarded with the table. Any step still parked on a withdrawn native is
-- soft-deleted by the startup prune (§12) and its parked price returns at process closure (§6),
-- the same path any step on a removed action already takes.
DROP TABLE IF EXISTS step_gates;
