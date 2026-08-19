-- Indexes for keyset paging over the event feed.
--
-- `list_events()` now orders by (event_time DESC, event_id DESC) and
-- accepts an exclusive `(event_time, event_id)` cursor, so the dashboard
-- can page backwards into history instead of being limited to the
-- in-memory ring the projection retains.
--
-- The existing single-column-plus-time indexes from 001_initial stop
-- short of that ordering: with only `(event_type, event_time DESC)`,
-- Postgres can use the index for the filter but must add a sort node to
-- resolve the `event_id DESC` tiebreak, over the whole matching set --
-- which is exactly the set a reader is paging through. Extending each
-- with `event_id DESC` keeps the read index-only at every page.
--
-- The tiebreak itself is not optional. `(event_time, event_id)` is the
-- table's PRIMARY KEY and therefore unique; `event_time` alone is not,
-- because burst writes routinely share a timestamp at microsecond
-- resolution. A keyset cursor over a non-unique key silently skips or
-- repeats whatever collided with it, and a reader who scrolled past the
-- gap gets no error. `get_agent_states()` already needed the same
-- tiebreak for the same reason (see 001_initial).
--
-- `category` is a new filter: the Activity view's category pills push
-- their selection into the query rather than filtering a paged list
-- client-side, because filtering after paging silently excludes -- a
-- 50-row page holding 2 matches reads as "only 2 exist". Without an
-- index that filter is a sequential scan over a hypertable, on the most
-- used control on the screen.

DROP INDEX IF EXISTS idx_crewlet_events_event_type_time;
CREATE INDEX IF NOT EXISTS idx_crewlet_events_event_type_time
    ON crewlet_events (event_type, event_time DESC, event_id DESC);

DROP INDEX IF EXISTS idx_crewlet_events_source_time;
CREATE INDEX IF NOT EXISTS idx_crewlet_events_source_time
    ON crewlet_events (source, event_time DESC, event_id DESC);

DROP INDEX IF EXISTS idx_crewlet_events_actor_time;
CREATE INDEX IF NOT EXISTS idx_crewlet_events_actor_time
    ON crewlet_events (actor, event_time DESC, event_id DESC);

CREATE INDEX IF NOT EXISTS idx_crewlet_events_category_time
    ON crewlet_events (category, event_time DESC, event_id DESC);
