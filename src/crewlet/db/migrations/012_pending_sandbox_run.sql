-- Code-runtime sandbox: durable state for a DETACHED Execute job.
--
-- A sandbox-backed Execute does not finish in its kick-off turn: the
-- turn starts a background coding job and ENDS; the job's completion
-- (or a human's answer to a mid-run clarification) arrives later as a
-- FRESH turn. Turns are otherwise pure in-memory, so the detached path
-- needs this row to survive: the completion turn rebuilds everything it
-- needs (plan, success criteria, trace context, where to report back)
-- from here, and a startup recovery pass re-attaches to still-live
-- sandboxes after an engine restart (without it a restart orphans the
-- sandbox — it runs to its TTL and nobody collects the result).
--
-- Identity is the kick-off turn_id (PRIMARY KEY). The status column is a
-- small state machine:
--   running                -> the background job is executing (agent busy)
--   awaiting_clarification -> the coding agent asked a person; agent FREE
--   resumed                -> the tail/answer was claimed (at-most-once gate)
--   done | failed          -> terminal
--   reseed                 -> paused sandbox reaped past pause_ttl; on the
--                             eventual answer, re-seed a fresh session from
--                             the git branch
--
-- The at-most-once tail guard is an atomic conditional flip:
--   UPDATE ... SET status='resumed' WHERE turn_id=$1
--     AND status IN ('running','awaiting_clarification') RETURNING turn_id
-- so a duplicate completion signal (or restart-then-resume) runs the
-- tail once.

CREATE TABLE pending_sandbox_run (
    turn_id           TEXT        PRIMARY KEY,
    agent_handle      TEXT        NOT NULL,
    agent_id          TEXT        NOT NULL DEFAULT '',
    role              TEXT        NOT NULL DEFAULT '',
    sandbox_id        TEXT        NOT NULL DEFAULT '',
    coding_agent      TEXT        NOT NULL DEFAULT '',
    command_id        TEXT        NOT NULL DEFAULT '',  -- background command / pid for reconnect
    status            TEXT        NOT NULL DEFAULT 'running'
                      CHECK (status IN ('running', 'awaiting_clarification',
                                        'resumed', 'done', 'failed', 'reseed')),
    plan              JSONB       NOT NULL DEFAULT '{}'::jsonb,  -- serialized ExecutionPlan
    task_description  TEXT        NOT NULL DEFAULT '',
    success_criteria  JSONB       NOT NULL DEFAULT '[]'::jsonb,
    conversation_key  TEXT        NOT NULL DEFAULT '',  -- where to report back / match answer
    notification_metadata JSONB   NOT NULL DEFAULT '{}'::jsonb,  -- trigger channel/thread for in-thread reply
    branch            TEXT        NOT NULL DEFAULT '',  -- pushed WIP branch (the durable state)
    session_id        TEXT        NOT NULL DEFAULT '',  -- coding-agent session (resume optimisation)
    question          TEXT        NOT NULL DEFAULT '',  -- set when awaiting_clarification
    audience          TEXT        NOT NULL DEFAULT '',  -- requester | team | manager | <handle>
    trace_id          TEXT        NOT NULL DEFAULT '',  -- OTel trace so the follow-up nests
    span_id           TEXT        NOT NULL DEFAULT '',
    budget_remaining  BIGINT      NOT NULL DEFAULT 0,   -- token-budget snapshot at kick-off
    delegation_depth  INT         NOT NULL DEFAULT 0,
    delegation_chain  JSONB       NOT NULL DEFAULT '[]'::jsonb,
    pause_ttl_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 0 = never pause, always re-seed
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Startup recovery + "is this agent busy?" both scan by status; the
-- waiter reaps paused/clarification rows by age.
CREATE INDEX idx_pending_sandbox_run_status ON pending_sandbox_run (status);
CREATE INDEX idx_pending_sandbox_run_agent ON pending_sandbox_run (agent_handle, status);
-- A clarification answer is matched back to its run by conversation_key.
CREATE INDEX idx_pending_sandbox_run_conversation
    ON pending_sandbox_run (conversation_key)
    WHERE status = 'awaiting_clarification';
