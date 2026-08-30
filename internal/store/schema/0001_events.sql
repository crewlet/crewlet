-- Crewlet local store — schema conventions.
--
-- This is the END STATE, not a replay of the Postgres tree's 31 forward
-- migrations. Those files are the inventory of what exists and why; the
-- history that produced them belongs to a database this binary will never
-- open: a deployment stands up fresh, with no data migration from it.
--
-- Every statement here must parse on BOTH certified drivers — Turso and
-- mainline SQLite — because Turso's dialect is the narrower of the two and
-- the dual-driver test job is the only thing that catches a divergence
-- (adrs/002). Three conventions follow from that, plus one
-- from the engine owning its own clock:
--
--   * TIMESTAMPTZ  -> INTEGER, microseconds since the Unix epoch, UTC.
--     Not TEXT. Keyset paging compares (event_time, event_id) as a tuple,
--     and ISO-8601 text does not order correctly across encodings: the
--     same instant written "…T12:00:00+00:00" sorts AFTER itself written
--     naive. Integers make that class of bug unrepresentable, and
--     microseconds is exactly the resolution TIMESTAMPTZ carried, so no
--     precision is traded for it.
--   * JSONB        -> TEXT holding JSON. Both drivers ship json1, so
--     json_extract() still reaches into a column when a query needs to.
--   * BOOLEAN      -> INTEGER 0/1.
--
-- No column defaults its own timestamp. Postgres could write now(); here
-- the engine supplies every instant, from one clock, so a row's time is
-- the time the engine meant rather than whatever the storage layer
-- happened to observe. A forgotten column is then a NOT NULL error at the
-- first test that writes it, which is louder — and far earlier — than a
-- row silently stamped 1970.

-- crewlet_events — the audit / observability event store.
--
-- One row per engine event worth keeping. The dimensions a reader filters
-- on (type, source, category, trace, agent, task, channel, sender) are
-- promoted to their own columns so a query never parses JSON to find a
-- row; everything else rides in `tags` and the serialized `payload`.
CREATE TABLE crewlet_events (
    -- (event_time, event_id) is the identity. event_time leads because
    -- every read of this table is time-ordered, and event_id is the
    -- tiebreak WITHOUT WHICH THE KEY IS NOT UNIQUE: burst writes share a
    -- timestamp at microsecond resolution routinely, and a keyset cursor
    -- over a non-unique key skips or repeats whatever collided with it —
    -- silently, with no error reaching the reader who scrolled past the
    -- gap.
    event_time     INTEGER NOT NULL,
    event_id       TEXT    NOT NULL,
    event_type     TEXT    NOT NULL,
    source         TEXT    NOT NULL DEFAULT '',
    category       TEXT    NOT NULL DEFAULT '',
    trace_id       TEXT    NOT NULL DEFAULT '',
    span_id        TEXT    NOT NULL DEFAULT '',
    parent_span_id TEXT    NOT NULL DEFAULT '',
    agent_id       TEXT    NOT NULL DEFAULT '',
    agent_role     TEXT    NOT NULL DEFAULT '',
    task_id        TEXT    NOT NULL DEFAULT '',
    channel_id     TEXT    NOT NULL DEFAULT '',
    sender         TEXT    NOT NULL DEFAULT '',
    summary        TEXT    NOT NULL DEFAULT '',
    actor          TEXT    NOT NULL DEFAULT '',
    tags           TEXT    NOT NULL DEFAULT '{}',
    payload        TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (event_time, event_id)
);

-- Every filter index ends (…, event_time DESC, event_id DESC) because
-- that is the listing's ORDER BY. Stopping at event_time — as the
-- Postgres tree's first cut did — lets the index serve the filter but
-- forces a sort over the whole matching set to resolve the id tiebreak,
-- which is precisely the set a reader is paging through.
CREATE INDEX crewlet_events_type_time_idx
    ON crewlet_events (event_type, event_time DESC, event_id DESC);
CREATE INDEX crewlet_events_source_time_idx
    ON crewlet_events (source, event_time DESC, event_id DESC);
CREATE INDEX crewlet_events_actor_time_idx
    ON crewlet_events (actor, event_time DESC, event_id DESC);
-- The Activity view's category pills push their selection into the query
-- rather than filtering a page client-side: filtering after paging
-- silently excludes, and a 50-row page holding 2 matches reads as
-- "only 2 exist".
CREATE INDEX crewlet_events_category_time_idx
    ON crewlet_events (category, event_time DESC, event_id DESC);

-- A trace is read the other way — oldest first, as a causal sequence —
-- so its index ascends.
CREATE INDEX crewlet_events_trace_idx
    ON crewlet_events (trace_id, event_time, event_id);

-- Lookup by id alone: the identity is (event_time, event_id), and a
-- caller holding only an id (a link, a pasted log line) has no time to
-- seek with.
CREATE INDEX crewlet_events_id_idx
    ON crewlet_events (event_id);

-- Per-seat reads. Both carry the event_id tiebreak for the same reason
-- the filter indexes do; the Postgres tree gave it to agent_role only,
-- which left the agent_id read (an agent's recent LLM records) ordered
-- nondeterministically among rows sharing a microsecond.
CREATE INDEX crewlet_events_agent_role_time_idx
    ON crewlet_events (agent_role, event_time DESC, event_id DESC);
CREATE INDEX crewlet_events_agent_id_time_idx
    ON crewlet_events (agent_id, event_time DESC, event_id DESC);

-- The retention sweep is a range delete on event_time, which the primary
-- key's leading column already serves — no extra index. That the sweep
-- EXISTS is the change: the Postgres table had no retention policy at
-- all, on the highest-volume table in the deployment, so it grew for the
-- life of the install while every read of it stopped at 30 days. See
-- EventRetention in the Go layer for how the floor is chosen.
