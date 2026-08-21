-- Crewlet initial schema.  Requires the ``vector`` and ``timescaledb``
-- extensions; the image the bundled ``docker-compose.yml`` runs ships with
-- both preloaded.  (That image, and the pg major it pins, are named there
-- and nowhere else -- this comment used to name a different one at a
-- different major, which is how a reader learns the wrong prerequisite.)

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Per-agent cumulative token consumption.
CREATE TABLE token_usage (
    agent_handle    TEXT PRIMARY KEY,
    tokens_used     BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Slack thread-follow state (per agent, per thread).  Survives restarts.
CREATE TABLE slack_thread_follows (
    agent_handle    TEXT NOT NULL,
    channel_id      TEXT NOT NULL,
    thread_ts       TEXT NOT NULL,
    reason          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_handle, channel_id, thread_ts)
);

CREATE INDEX idx_slack_thread_follows_thread
    ON slack_thread_follows(channel_id, thread_ts);

-- Observability event store (TimescaleDB hypertable).
CREATE TABLE crewlet_events (
    -- ``event_time`` is first in the PK so the hypertable chunks by
    -- time range; ``event_id`` is the tiebreaker so the PK still
    -- uniquely identifies a row across chunks.
    event_time     TIMESTAMPTZ NOT NULL,
    event_id       TEXT        NOT NULL,
    event_type     TEXT        NOT NULL,
    source         TEXT        NOT NULL DEFAULT '',
    category       TEXT        NOT NULL DEFAULT '',
    trace_id       TEXT        NOT NULL DEFAULT '',
    span_id        TEXT        NOT NULL DEFAULT '',
    parent_span_id TEXT        NOT NULL DEFAULT '',
    agent_id       TEXT        NOT NULL DEFAULT '',
    agent_role     TEXT        NOT NULL DEFAULT '',
    task_id        TEXT        NOT NULL DEFAULT '',
    channel_id     TEXT        NOT NULL DEFAULT '',
    sender         TEXT        NOT NULL DEFAULT '',
    summary        TEXT        NOT NULL DEFAULT '',
    actor          TEXT        NOT NULL DEFAULT '',
    tags           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    payload        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (event_time, event_id)
);

SELECT create_hypertable(
    'crewlet_events',
    'event_time',
    if_not_exists => TRUE,
    migrate_data  => TRUE
);

CREATE INDEX idx_crewlet_events_event_type_time
    ON crewlet_events (event_type, event_time DESC);
CREATE INDEX idx_crewlet_events_source_time
    ON crewlet_events (source, event_time DESC);
CREATE INDEX idx_crewlet_events_actor_time
    ON crewlet_events (actor, event_time DESC);
-- ``list_trace()`` orders by (event_time ASC, event_id ASC) after
-- filtering by trace_id.  Putting ``event_id`` in the index lets
-- Postgres serve the ORDER BY directly from the index instead of
-- adding a sort node on top of the trace_id scan.
CREATE INDEX idx_crewlet_events_trace_id_time
    ON crewlet_events (trace_id, event_time, event_id);
CREATE INDEX idx_crewlet_events_event_id
    ON crewlet_events (event_id);
-- ``get_agent_states()`` orders by (event_time DESC, event_id DESC)
-- after filtering by agent_role, so ``event_id DESC`` must be in the
-- index for the sort step to be elided.
CREATE INDEX idx_crewlet_events_agent_role_time
    ON crewlet_events (agent_role, event_time DESC, event_id DESC);
CREATE INDEX idx_crewlet_events_agent_id_time
    ON crewlet_events (agent_id, event_time DESC);
