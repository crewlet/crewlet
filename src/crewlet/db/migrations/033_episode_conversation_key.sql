-- episodes.conversation_key — which conversation a turn served.
--
-- An episode already records WHAT a turn did; until now nothing recorded
-- WHERE. Retrieval is agent-scoped cosine similarity over
-- `task_summary | plan_summary` with no conversation filter, so "the
-- previous turn on this same ticket" surfaced only by similarity luck.
--
-- The value is `{source}:{local}` from
-- crewlet.notifications.coalesce.conversation_key — the identity that
-- already partitions every seat inbox for coalescing, and that
-- `pending_sandbox_run` already persists to match a human's answer back
-- to a parked run. One derivation, now stamped where turns are recorded.
--
-- Empty for triggers with no derivable conversation (a scheduled fire, a
-- task assignment, an A2A wake — those key as `event:{id}`, which no
-- later message can reproduce, so storing one would be noise), and empty
-- on compacted rows, whose cluster spans conversations by construction.
--
-- Mixed-version safe: an older build never selects the column and
-- inserts the default, which reads exactly as "no conversation known".

ALTER TABLE episodes
    ADD COLUMN IF NOT EXISTS conversation_key TEXT NOT NULL DEFAULT '';

-- Partial, and per agent for the same reason work_key's index is: two
-- seats legitimately serve one conversation, and each one's episodes are
-- its own memory. Filtered on non-empty because the majority of rows in
-- a schedule-driven company carry no conversation at all.
CREATE INDEX IF NOT EXISTS episodes_agent_conversation_idx
    ON episodes (agent_handle, conversation_key)
    WHERE conversation_key <> '';
