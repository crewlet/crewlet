-- A turn's id starts naming ONE RUN, and the unit of work gets a column of
-- its own.
--
-- `turn_id` has carried the WORK KEY since it was promoted out of the payload
-- in 0015 — the digest of the trigger events a dispatch derived the turn from,
-- which is deliberately stable across a re-run (see internal/workkey). That is
-- the right identity for collapsing a duplicate and the wrong one for naming
-- an execution, and every reader here takes the second meaning:
--
--   * turnlist.go GROUPs BY turn_id, so two attempts at one trigger folded
--     into one row whose `failed` was MAX over both — a turn that failed on
--     auth, was redelivered and then succeeded read as permanently failed,
--     with the two attempts' tokens summed and their models concatenated.
--   * the phase record's identity is (turn_id, phase, iteration), so a retry
--     republished the identities the failed attempt already occupied: the
--     live projection folded the retry's rounds into the previous attempt's
--     frozen failed call, and the dashboard's merge kept the older row, so
--     the retry was invisible for as long as it ran.
--
-- So `turn_id` now holds a per-run uuid and `work_key` holds what it used to.
-- See ADR-0017.
ALTER TABLE crewlet_events ADD COLUMN work_key TEXT NOT NULL DEFAULT '';

-- BACKFILLED FROM turn_id, because that IS what these rows' turn ids were.
-- Every row written before this migration carries a work key in that column,
-- so copying it across is not an approximation — it is the value moving to the
-- column that now means it. The old rows keep grouping exactly as they did:
-- one row per unit of work, because that build could not tell the difference.
--
-- Scoped to rows that have one. `turn_id` is `NOT NULL DEFAULT ''` and most
-- rows in this table are not turn events at all, and a work key of '' is the
-- documented "a turn with no ledgerable trigger" value — so writing '' to ''
-- would be work with no reader.
--
-- THE COLUMN ONLY, and not the `tags` blob beside it. That blob is what the
-- WRITER extracted from an event's own JSON at the time it was written, and
-- those events genuinely carried no `work_key` field — rewriting every one of
-- them would restate history, re-encode the largest text column in the table
-- on a boot, and still leave the stored payload disagreeing with it. So the
-- COLUMN is the authority every reader takes: `EventRecord.WorkKey` is read
-- off it rather than out of the tags, which is the one promoted value that is
-- not a copy of a tag. A reader going through the tags answers "no unit of
-- work" for exactly the rows this backfill exists to preserve.
--
-- ONE STATEMENT, UNBATCHED, and that is a deliberate difference from the
-- retention sweep, which batches over this same table for a reason that does
-- not apply here. Purge runs on a tick while the publish listener is appending
-- INLINE on the publishing goroutine, so a long write transaction starves live
-- appends past busy_timeout and the events are dropped. A migration runs
-- inside Open, before this node serves anything and before it has a broker
-- connection, so there is nothing to starve — what it costs is boot time on a
-- large log, paid once. The alternative is a work-key filter that answers
-- nothing for the history this store already holds.
UPDATE crewlet_events SET work_key = turn_id WHERE turn_id <> '';

-- "Every attempt at this trigger" is the question the split creates, and
-- without an index it is a scan of the largest table in the store — the same
-- reasoning 0018 gives for the turn index, which this mirrors down to the
-- (event_time, event_id) tail that answers the ordering every listing uses.
--
-- PARTIAL over `work_key <> ''`, like conversation_sessions' own work-key
-- index in 0005: '' is not an identity, it is the absence of one, and an index
-- entry per non-turn row would be most of the table pointing at nothing.
CREATE INDEX IF NOT EXISTS crewlet_events_work_key_idx
    ON crewlet_events (work_key, event_time, event_id)
    WHERE work_key <> '';
