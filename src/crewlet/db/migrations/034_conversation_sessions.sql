-- conversation_sessions — what a seat already did in one conversation.
--
-- The per-turn record the turn engine renders back into the NEXT turn of
-- the same Slack thread / Jira issue / GitHub PR. See
-- crewlet.db.conversation_sessions for why this is not episodes: episode
-- recall is agent-scoped cosine similarity over two summaries, with no
-- conversation filter and no reply text, and it is gated OFF for exactly
-- the thin pointer-shaped triggers that continue a conversation.
--
-- The dedupe key is (agent_handle, conversation_key, work_key), NOT
-- turn_id: two nodes completing one trigger mint two turn ids, so a
-- turn-keyed unique index would record the duplicate instead of
-- collapsing it. work_key is the constituent-trigger-event identity the
-- completion ledger and episodes already key on.
--
-- Unlike episodes, this is a REGULAR table, not a hypertable — so the
-- dedupe is a plain unique index and an ordinary ON CONFLICT DO NOTHING.
-- The advisory-lock + WHERE NOT EXISTS dance in 031_work_key.sql exists
-- only because a hypertable forbids a unique index that omits its time
-- column; nothing here needs it.

CREATE TABLE IF NOT EXISTS conversation_sessions (
    agent_handle     TEXT        NOT NULL,
    conversation_key TEXT        NOT NULL,
    work_key         TEXT        NOT NULL,
    turn_id          TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    entry            JSONB       NOT NULL DEFAULT '{}'::jsonb
);

-- Dedupe: first writer wins for one unit of work in one conversation.
CREATE UNIQUE INDEX IF NOT EXISTS conversation_sessions_dedupe_idx
    ON conversation_sessions (agent_handle, conversation_key, work_key);

-- The read path: newest N entries of one conversation, and the
-- write-time trim that bounds a DM channel (whose conversation key is
-- the whole channel rather than a thread, so it never stops growing).
CREATE INDEX IF NOT EXISTS conversation_sessions_read_idx
    ON conversation_sessions (agent_handle, conversation_key, created_at DESC);

-- Retention is a range delete over created_at, run by the
-- MaintenanceWorker; without this index it degrades to a full scan.
CREATE INDEX IF NOT EXISTS conversation_sessions_created_at_idx
    ON conversation_sessions (created_at);
