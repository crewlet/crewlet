-- What started a turn becomes a column of the event log: the object the waking
-- event was about, as `<kind>:<id>` (a chat message, a work item, a schedule,
-- a colleague's ask).
--
-- WHAT A COLUMN BUYS HERE is the question "which turn did this message start".
-- Every record a turn publishes carries its trigger, and since this build the
-- trigger names its origin (types.TriggerOrigin): the chat message a person
-- wrote, under the reason it reached the seat. A filter on it through the
-- payload is a json_extract over every row in the window, which is what
-- schema/0015 exists to stop the reads here doing; the column is a copy of the
-- `trigger_origin` tag store.ExtractTags composes, as work_item is (0033).
--
-- An empty string, never NULL, for a row whose trigger names no origin, so a
-- reader compares against '' and the partial index below leaves those rows out.
-- No backfill: no record written before this build names an origin.
ALTER TABLE crewlet_events ADD COLUMN trigger_origin TEXT NOT NULL DEFAULT '';

-- THE TURNS AN OBJECT STARTED ARE A RANGE SEEK, in the order the turns list's
-- origin filter reads. PARTIAL over the rows that name one, the shape 0029 and
-- 0033 gave theirs.
CREATE INDEX IF NOT EXISTS crewlet_events_trigger_origin_idx
    ON crewlet_events (trigger_origin, event_time, event_id)
    WHERE trigger_origin <> '';
