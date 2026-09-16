-- Who an event involves becomes a queryable fact.
--
-- The dashboard's "show me this seat's activity" filter matched an event's
-- actor column plus four keys inside the tags JSON blob — and no index covers
-- a JSON blob. So it could not be a WHERE clause at all: the query fetched
-- five pages of raw history for every page it wanted, shipped them to Go,
-- decoded 500 tag documents, kept the ones that matched, and asked again if
-- that was not enough. Its termination condition was a page with nothing in
-- it, so a QUIET seat in a busy org was the worst case rather than the
-- cheapest one: the reader walked the entire thirty-day log, in 500-row
-- chunks, to conclude there was nothing to show.
--
-- # Why a table and not more columns
--
-- The obvious fix is to promote `target` and `recipient` beside the columns
-- `agent_role` and `sender` already have, and write
-- `WHERE actor = ? OR agent_role = ? OR target = ? OR ...`.
--
-- MEASURED AGAINST THIS ENGINE, THAT DOES NOT WORK. Its planner does no
-- OR-optimization: a single-column equality uses its index, and the same
-- predicate repeated across five columns with OR falls back to a full table
-- scan (EXPLAIN QUERY PLAN: `SCAN e`). Every index those columns carried
-- would be write amplification bought for nothing.
--
-- One row per (party, event) makes it a single indexed range scan instead —
-- `SEARCH p USING COVERING INDEX (party=?)`, then a primary-key seek back
-- into the log per match. The cost of the read stops scaling with the size of
-- the log and starts scaling with the size of the ANSWER, which is what was
-- wrong with it.

-- The primary key IS the index, and its column order is the query: seek to a
-- party, then walk that party's events newest-first, which is the order every
-- reader of this log wants. DESC in the key rather than a second index,
-- because a reverse scan of the same key would work equally well but leaves
-- the intent to be inferred.
CREATE TABLE crewlet_event_parties (
    party      TEXT    NOT NULL,
    event_time INTEGER NOT NULL,
    event_id   TEXT    NOT NULL,
    PRIMARY KEY (party, event_time DESC, event_id DESC)
);

-- SWEPT WITH THE LOG IT INDEXES, on the same horizon and in the same call —
-- see EventLog.Purge. A table that indexes a swept table and is not swept
-- itself grows for the life of the deployment while pointing at rows that no
-- longer exist, and every one of those rows is a primary-key seek that finds
-- nothing.
CREATE INDEX crewlet_event_parties_time_idx
    ON crewlet_event_parties (event_time);

-- BACKFILLED, so the filter keeps working over history an upgrade inherits.
-- Without this the seat view would be empty for everything written before
-- this migration, which reads as "this seat has never done anything" rather
-- than as a missing index.
--
-- Five statements rather than one with a UNION, because each is a plain scan
-- with a different projection and the engine's planner is happier with them
-- separately. Conflicts are ignored: an event whose actor is also its sender
-- names that party once.
INSERT INTO crewlet_event_parties (party, event_time, event_id)
SELECT actor, event_time, event_id FROM crewlet_events WHERE actor <> ''
ON CONFLICT (party, event_time, event_id) DO NOTHING;

INSERT INTO crewlet_event_parties (party, event_time, event_id)
SELECT agent_role, event_time, event_id FROM crewlet_events WHERE agent_role <> ''
ON CONFLICT (party, event_time, event_id) DO NOTHING;

INSERT INTO crewlet_event_parties (party, event_time, event_id)
SELECT sender, event_time, event_id FROM crewlet_events WHERE sender <> ''
ON CONFLICT (party, event_time, event_id) DO NOTHING;

INSERT INTO crewlet_event_parties (party, event_time, event_id)
SELECT json_extract(tags, '$.target'), event_time, event_id FROM crewlet_events
WHERE json_extract(tags, '$.target') IS NOT NULL
  AND json_extract(tags, '$.target') <> ''
ON CONFLICT (party, event_time, event_id) DO NOTHING;

INSERT INTO crewlet_event_parties (party, event_time, event_id)
SELECT json_extract(tags, '$.recipient'), event_time, event_id FROM crewlet_events
WHERE json_extract(tags, '$.recipient') IS NOT NULL
  AND json_extract(tags, '$.recipient') <> ''
ON CONFLICT (party, event_time, event_id) DO NOTHING;
