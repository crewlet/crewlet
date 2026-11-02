-- What started a turn becomes an index of the event log: every object the
-- turn's start named, as `<kind>:<id>` (a chat message, a work item, a
-- schedule, a colleague's ask), beside the turn it started.
--
-- WHAT IT BUYS is the question "which turn did this message start". Since this
-- build a turn's trigger names its origin (types.TriggerOrigin): the chat
-- message a person wrote, under the reason it reached the seat. A COALESCED
-- trigger names every message it took in (Trigger.Origins), because one turn
-- answered all of them, and each of those messages has to find it: the busy
-- case, a burst merged into one turn, is exactly where a person asks.
--
-- A TABLE RATHER THAN A COLUMN for that reason: one record names several
-- objects. It is written with the turn's own start record (agent_turn_started,
-- one per segment, so a turn resumed by a person's answer is found by that
-- answer too) in the same transaction as the event, by EventLog.Append, as the
-- party index (0016) is; not with every phase record, which repeat the same
-- trigger.
--
-- LED BY (origin, turn_id), which is the seek: the turns one object started.
-- The event's own identity completes the key, so a row belongs to one record
-- and goes with it: on the retention horizon (EventLog.Purge, on the time
-- index below) and when custody moves a record away.
CREATE TABLE crewlet_turn_origins (
    origin     TEXT    NOT NULL,
    turn_id    TEXT    NOT NULL,
    event_time INTEGER NOT NULL,
    event_id   TEXT    NOT NULL,
    PRIMARY KEY (origin, turn_id, event_time, event_id)
);

CREATE INDEX crewlet_turn_origins_time_idx ON crewlet_turn_origins (event_time);
