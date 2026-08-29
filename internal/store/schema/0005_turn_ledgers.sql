-- The three ledgers the turn engine reads before working and writes after:
-- what it already did (turn_completions), what it already said in this
-- conversation (conversation_sessions), and who it is currently asking
-- (a2a_channels).
--
-- Two of the three have since left this database for the fleet's coordination
-- store, and the migration that moved each carries the reason: turn_completions
-- in 0010, a2a_channels in 0012. The statements below stay as written — a
-- migration is a historical record, not a description of the current schema.


-- turn_completions — "has this trigger already been worked?"
--
-- NOT a claim. There is no in_progress state, no expiry and no supersede
-- rule, because the seat lease is ALREADY the mutual exclusion: only one
-- node consumes a seat's inbox at a time. A claim's only honest
-- disposition for a stale row is therefore "re-run", which is exactly
-- what you do with no row at all — so the extra states would buy nothing
-- and cost a lease's worth of edge cases.
--
-- Keyed on the CONSTITUENT event ids, never on a turn id. Two nodes
-- completing one trigger mint two turn ids, so a key from one RECORDS the
-- duplicate instead of collapsing it. A coalesced digest is minted fresh
-- on every merge and would key to nothing.
--
-- Polarity: BOTH DIRECTIONS FAIL OPEN. Chosen for this contract rather
-- than inherited from a house default — every store here picks its own,
-- and normalising them to one direction is the mistake. Not knowing
-- whether work was done has one safe answer and it is the pre-ledger one
-- — do the work. A read that fails closed would make a database blip
-- look like a company that had already answered everything.
CREATE TABLE turn_completions (
    -- The work key: a digest over the constituent trigger event ids.
    work_key     TEXT    NOT NULL,
    agent_handle TEXT    NOT NULL,
    turn_id      TEXT    NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, work_key)
);

-- The retention sweep is a range delete over completed_at. Its floor is
-- the scheduler's catchup window, NOT a round number: deleting a row a
-- tick could still evaluate lets that fire run twice, which is the one
-- failure this table exists to prevent.
CREATE INDEX turn_completions_completed_at_idx
    ON turn_completions (completed_at);


-- conversation_sessions — what this seat already said in ONE thread,
-- issue or channel, appended at turn end and rendered back into that
-- conversation's next turn.
--
-- Deliberately NOT episodes. Those are agent-scoped cosine similarity
-- over two summaries, carry no reply text, and are gated OFF on exactly
-- the thin pointer triggers that CONTINUE a conversation. A regular
-- table, so dedupe is a plain unique index rather than the advisory lock
-- a hypertable would force.
--
-- Bounded twice, because one bound is not enough: a per-conversation
-- trim on write (a chat DM keys on the whole CHANNEL, so it never stops
-- growing) and a retention sweep for conversations nobody returns to.
--
-- Polarity is SPLIT and deliberately so: writes fail open, reads RAISE.
-- Swallowing a read failure made "unreadable" and "nothing said yet" one
-- answer, and a screen drew a database outage as a silent seat.
CREATE TABLE conversation_sessions (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_handle     TEXT    NOT NULL,
    -- The surface-scoped conversation identity: a thread, an issue, a
    -- channel. Opaque here; the notification layer owns its grammar.
    conversation_key TEXT    NOT NULL,
    -- Deduped on the WORK key, never the turn id — same reason as
    -- turn_completions above.
    work_key         TEXT    NOT NULL DEFAULT '',
    turn_id          TEXT    NOT NULL DEFAULT '',
    -- The rendered SessionEntry. Budgets are applied once, at write time,
    -- so no reader has to know them and a later reader cannot restate
    -- history against a surface that has since changed.
    entry            TEXT    NOT NULL,
    created_at       INTEGER NOT NULL
);

-- The dedupe. Partial, because '' is the documented "a turn with no
-- ledgerable trigger": those skip the guard entirely and must not all
-- collide onto one row.
CREATE UNIQUE INDEX conversation_sessions_work_key_idx
    ON conversation_sessions (agent_handle, conversation_key, work_key)
    WHERE work_key <> '';

-- The read path: newest-first within one conversation.
CREATE INDEX conversation_sessions_lookup_idx
    ON conversation_sessions (agent_handle, conversation_key, created_at DESC);

CREATE INDEX conversation_sessions_created_at_idx
    ON conversation_sessions (created_at);


-- a2a_channels — the participants and open/closed state every
-- agent-to-agent authorization decision reads.
--
-- The channel is the AUTHORIZATION RECORD, not a transport. There is no
-- message queue here: the brief travels on the wake event and the answer
-- travels on the reply, both over the durable inbox. An in-process bus
-- carried them once, which meant the content existed on exactly one node
-- while the wake was delivered to whichever node owned the target's seat
-- — the same node only by luck.
CREATE TABLE a2a_channels (
    channel_id    TEXT    NOT NULL PRIMARY KEY,
    requester     TEXT    NOT NULL,
    target        TEXT    NOT NULL,
    -- Message COUNT, not messages. One ask and one answer is the whole
    -- protocol; the count is what makes a channel that saw more than two
    -- visible as the anomaly it is.
    message_count INTEGER NOT NULL DEFAULT 0,
    opened_at     INTEGER NOT NULL,
    -- NULL while open. A nullable timestamp rather than a status column
    -- so "when did it close" and "is it closed" cannot disagree.
    closed_at     INTEGER,
    last_at       INTEGER NOT NULL
);

-- The idle sweep closes channels no turn ever answered, and the
-- retention sweep drops the closed ones. Both are range scans over these.
CREATE INDEX a2a_channels_open_idx
    ON a2a_channels (last_at) WHERE closed_at IS NULL;

CREATE INDEX a2a_channels_closed_idx
    ON a2a_channels (closed_at) WHERE closed_at IS NOT NULL;
