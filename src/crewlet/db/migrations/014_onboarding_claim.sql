-- Cross-process single-flight for the first-turn onboarding pass.
--
-- The in-memory latch + lock on AgentInstance serialize onboarding within
-- ONE engine process, but agent inboxes are Shared subscriptions: during a
-- rolling restart/handover two engines can each run a turn for the same
-- (un-onboarded) agent and both start a full onboarding pass.  The pass
-- now takes a DB lease first: ``try_claim_pass`` upserts the agent's row
-- with ``in_progress_until = now() + ttl`` iff no live lease exists, and
-- the loser skips its pass (the winner's mark suppresses every later one).
--
-- The lease lives on the marker row itself (a claim for a never-marked
-- agent inserts the row with an empty chain_hash, which ``is_onboarded``
-- correctly reads as "not onboarded").  The TTL bounds a crashed
-- claimant: a stale lease expires and the next turn re-claims.

ALTER TABLE agent_onboarding_markers
    ADD COLUMN in_progress_until TIMESTAMPTZ;
