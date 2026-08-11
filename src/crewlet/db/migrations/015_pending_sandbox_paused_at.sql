-- Code-runtime sandbox: make a PAUSED box's age durable so it can be reaped.
--
-- A sandbox blocked on a person's answer is paused, not killed: the
-- coding agent's session + working tree survive, so an answer that arrives
-- soon resumes exactly where it stopped. But E2B holds a paused sandbox
-- INDEFINITELY -- there is no provider-side TTL -- and bills for the
-- snapshot, so expiring it is the engine's job. `pause_ttl_seconds` (already
-- on the row) says how long to hold it; this column says since when, which
-- is what the SandboxWaiter's reaper compares against.
--
-- Invariant this establishes for the row:
--   sandbox_id <> ''   <=> the engine believes a box exists for this run
--   paused_at IS NOT NULL <=> that box is currently paused
-- Reaping clears BOTH (the box is gone), which is also what makes the
-- eventual re-seed provision a fresh box instead of reattaching to a dead
-- sandbox id.
--
-- `updated_at` deliberately is NOT reused for this: it moves on every write
-- to the row (status flips, execute_state saves), so a TTL measured from it
-- would silently reset and a paused box could outlive its TTL indefinitely.

ALTER TABLE pending_sandbox_run
    ADD COLUMN paused_at TIMESTAMPTZ;

-- The reaper scans paused rows every waiter tick; keep it off a seq scan.
CREATE INDEX idx_pending_sandbox_run_paused
    ON pending_sandbox_run (paused_at)
    WHERE paused_at IS NOT NULL;

-- A clarification answer is matched back to its run by conversation_key --
-- and a run whose paused box was already reaped ('reseed') is still waiting
-- for exactly that answer, so it must stay matchable.
DROP INDEX idx_pending_sandbox_run_conversation;
CREATE INDEX idx_pending_sandbox_run_conversation
    ON pending_sandbox_run (conversation_key)
    WHERE status IN ('awaiting_clarification', 'reseed');
