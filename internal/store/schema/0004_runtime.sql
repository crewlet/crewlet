-- Runtime state that must outlive the process that created it: the
-- schedule dispatch ledger, detached sandbox runs, chat thread follows,
-- and cumulative token usage.

-- scheduled_runs — the dispatch ledger for role- and unit-scoped
-- recurring work. One row per fired (or deliberately skipped) run;
-- the composite key plus INSERT … ON CONFLICT DO NOTHING is what makes
-- delivery at-most-once across restarts and re-ticks.
--
-- Identity is the tuple of columns, NOT a delimiter-joined string, so a
-- ':' in a unit name, schedule name or handle cannot collide two distinct
-- fires onto one key.
CREATE TABLE scheduled_runs (
    scope_type    TEXT    NOT NULL,
    scope_id      TEXT    NOT NULL,
    schedule_name TEXT    NOT NULL,
    -- The LOCAL wall-clock stamp (YYYYmmddTHHMM in the schedule's own
    -- timezone), not the UTC instant, and that is what makes the dedupe
    -- DST-correct: on a fall-back day one local cron minute maps to two
    -- UTC instants, but both share a fire_label, so the run fires once.
    fire_label    TEXT    NOT NULL,
    -- '' for a skipped catchup, which resolved no runner.
    target_handle TEXT    NOT NULL DEFAULT '',
    -- The UTC instant of the first-claimed fire, kept for display.
    scheduled_at  INTEGER NOT NULL,
    fired_at      INTEGER NOT NULL,
    -- 'fired' | 'skipped_catchup'. This is a DISPATCH ledger, not a
    -- turn-outcome store: whether the resulting turn finished, failed or
    -- timed out lives in the normal turn telemetry under the same trace.
    outcome       TEXT    NOT NULL DEFAULT 'fired',
    trace_id      TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (scope_type, scope_id, schedule_name, fire_label, target_handle)
);

-- The dashboard's schedules view reads the most recent fires.
CREATE INDEX scheduled_runs_fired_at_idx
    ON scheduled_runs (fired_at DESC);


-- pending_sandbox_run — durable state for a DETACHED Execute job.
--
-- A sandbox-backed Execute does not finish in its kick-off turn: the turn
-- starts a background coding job and ENDS, and the job's completion (or a
-- human's answer to a mid-run question) arrives later. Turns are
-- otherwise pure in-memory, so this row is what survives: the resume
-- rebuilds plan, criteria, trace context and where to report back from
-- here, and a startup pass re-attaches to boxes that outlived the engine.
-- Without it a restart orphans the sandbox — it runs to its TTL, is
-- billed for, and nobody collects the result.
CREATE TABLE pending_sandbox_run (
    -- The kick-off turn's id is the identity.
    turn_id           TEXT    NOT NULL PRIMARY KEY,
    agent_handle      TEXT    NOT NULL,
    agent_id          TEXT    NOT NULL DEFAULT '',
    role              TEXT    NOT NULL DEFAULT '',
    -- Non-empty exactly when the engine believes a box exists for this
    -- run; paused_at non-NULL exactly when that box is paused. Reaping
    -- clears BOTH, which is what makes the eventual re-seed provision a
    -- fresh box instead of reattaching to a dead sandbox id.
    sandbox_id        TEXT    NOT NULL DEFAULT '',
    coding_agent      TEXT    NOT NULL DEFAULT '',
    -- Background command / pid, for reconnect after a restart.
    command_id        TEXT    NOT NULL DEFAULT '',
    --   running                the job is executing; the agent is busy
    --   awaiting_clarification it asked a person; the agent is FREE
    --   resumed               the tail was claimed (at-most-once gate)
    --   done | failed         terminal
    --   reseed                the paused box was reaped past its TTL; on
    --                         the eventual answer, re-seed from the
    --                         pushed branch
    -- The at-most-once tail guard is a conditional flip to 'resumed'
    -- WHERE status IN ('running','awaiting_clarification'), so a
    -- duplicate completion signal runs the tail once.
    status            TEXT    NOT NULL DEFAULT 'running'
                      CHECK (status IN ('running', 'awaiting_clarification',
                                        'resumed', 'done', 'failed', 'reseed')),
    plan              TEXT    NOT NULL DEFAULT '{}',
    task_description  TEXT    NOT NULL DEFAULT '',
    success_criteria  TEXT    NOT NULL DEFAULT '[]',
    -- Where to report back, and what a later human answer is matched to.
    conversation_key  TEXT    NOT NULL DEFAULT '',
    notification_metadata TEXT NOT NULL DEFAULT '{}',
    -- The pushed WIP branch: the durable half of the run's work.
    branch            TEXT    NOT NULL DEFAULT '',
    session_id        TEXT    NOT NULL DEFAULT '',
    question          TEXT    NOT NULL DEFAULT '',
    audience          TEXT    NOT NULL DEFAULT '',
    trace_id          TEXT    NOT NULL DEFAULT '',
    span_id           TEXT    NOT NULL DEFAULT '',
    budget_remaining  INTEGER NOT NULL DEFAULT 0,
    delegation_depth  INTEGER NOT NULL DEFAULT 0,
    delegation_chain  TEXT    NOT NULL DEFAULT '[]',
    -- 0 means never pause, always re-seed.
    pause_ttl_seconds REAL    NOT NULL DEFAULT 0,
    -- The suspended Execute conversation: message list (including the
    -- assistant turn whose run_sandbox tool_use is still unanswered),
    -- the dangling call's id and name, the active tool surface, the
    -- loaded skill keys, the iteration label and the partial token
    -- counts. One blob, so adding a field to the suspended state is not
    -- a schema change.
    execute_state     TEXT    NOT NULL DEFAULT '{}',
    -- Deliberately NOT derived from updated_at: that column moves on
    -- every write to the row (status flips, execute_state saves), so a
    -- TTL measured from it silently resets and a paused box outlives its
    -- TTL indefinitely. E2B holds a paused box forever and bills for the
    -- snapshot, so expiring it is the engine's job.
    paused_at         INTEGER,
    -- The seat's owner at claim time — a process INCARNATION
    -- ({node_id}:{random}), so a restarted node with the same node_id
    -- does not inherit its predecessor's claim — and the seat lease's
    -- monotonic epoch as the fencing token. Every mutation on a live run
    -- carries WHERE owner_epoch = $mine: the ownership check in the
    -- handler is an optimisation, the fence in the write is the
    -- guarantee. NULL means unclaimed, which is what an in-flight run
    -- looks like the instant before its seat's owner recovers it.
    owner             TEXT,
    owner_epoch       INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

-- Startup recovery and "is this agent busy?" both scan by status.
CREATE INDEX pending_sandbox_run_status_idx
    ON pending_sandbox_run (status);
CREATE INDEX pending_sandbox_run_agent_idx
    ON pending_sandbox_run (agent_handle, status);

-- A clarification answer is matched back to its run by conversation_key
-- — and a run whose paused box was already reaped ('reseed') is still
-- waiting for exactly that answer, so it must stay matchable.
CREATE INDEX pending_sandbox_run_conversation_idx
    ON pending_sandbox_run (conversation_key)
    WHERE status IN ('awaiting_clarification', 'reseed');

-- The reaper scans paused rows every waiter tick; keep it off a full scan.
CREATE INDEX pending_sandbox_run_paused_idx
    ON pending_sandbox_run (paused_at)
    WHERE paused_at IS NOT NULL;


-- chat_thread_follows — thread-follow state for EVERY chat backend.
--
-- An agent follows a thread identified by (channel, thread); top-level
-- messages always deliver, replies deliver only in followed threads.
--
-- `backend` is part of the PRIMARY KEY rather than a tag, because thread
-- ids are unique only WITHIN a backend: a Mattermost root post id and a
-- Slack thread_ts come from different namespaces and could collide for
-- the same agent. Without it in the key, one backend's follow row could
-- satisfy another backend's lookup.
CREATE TABLE chat_thread_follows (
    backend      TEXT    NOT NULL,
    agent_handle TEXT    NOT NULL,
    channel_id   TEXT    NOT NULL,
    thread_id    TEXT    NOT NULL,
    reason       TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    -- Refreshed on every re-assert (a mention, a collective address, the
    -- agent's own post), so this is a true last-activity stamp rather
    -- than a creation date — which is what makes it the right column for
    -- the retention sweep to range over.
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (backend, agent_handle, channel_id, thread_id)
);

CREATE INDEX chat_thread_follows_thread_idx
    ON chat_thread_follows (backend, channel_id, thread_id);

-- Retention is a range delete over updated_at; without this index it
-- degrades to a full scan of a table read on the hot path of every
-- inbound chat message.
CREATE INDEX chat_thread_follows_updated_at_idx
    ON chat_thread_follows (updated_at);


-- token_usage — per-agent cumulative token consumption.
--
-- The durable counter, distinct from the per-run in-memory meter: budget
-- enforcement reads this, so it survives a restart and is shared across
-- every node. Its polarity is FAIL CLOSED (REWRITE_PLAN §15) — a spend
-- that cannot be recorded must be refused, because an unbilled turn
-- charges nothing and the next one charges nothing either.
CREATE TABLE token_usage (
    agent_handle TEXT    NOT NULL PRIMARY KEY,
    tokens_used  INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL
);
