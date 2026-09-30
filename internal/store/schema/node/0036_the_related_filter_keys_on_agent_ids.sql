-- The related-agent index is keyed on agent ids, never names.
--
-- # What was wrong
--
-- 0016 made "every event involving this agent" an index seek by writing one
-- `crewlet_event_parties` row per (party, event). What it wrote as a party was
-- a NAME: the event's actor, its `agent_role` tag — a seat's display name —
-- and the `target`, `recipient` and `sender` tags as they came. A seat's name
-- is prose two seats may share, so `GET /events?agent=` listed every
-- namesake's work as one seat's; a handle a seat had given up went on
-- answering for whichever seat the chart handed it to next; and an external
-- sender who happened to share a seat's handle was filed under that seat.
--
-- # What a party is now
--
-- An AGENT ID, which is one seat's for the life of the company (ADR-0019):
-- the event's own `agent_id`, and each seat a participant tag names, resolved
-- through the org chart by the process that writes the event — see
-- internal/store's `partiesOf`. The filter's `agent` parameter is resolved to
-- an agent id the same way before it is compared.
--
-- # Why the old rows go, and what the history keeps
--
-- Every row this table held is a name, and no id will ever equal one: kept,
-- they would be dead weight on the index for the rest of the retention
-- window, answering nothing. They are replaced from the one party the log
-- carries AS an id — each event's own `agent_id` — so an agent's own history
-- stays in the filter across the upgrade. The participant tags cannot be
-- carried over: resolving a handle to an agent id needs the company's name and
-- the seat each handle named when the event was written, and neither is in
-- this database. An A2A counterpart or a delivery's recipient named before
-- this migration drops out of the related view for the rest of the thirty-day
-- window; everything written after it is filed under both ends.
--
-- 0016 IS NOT EDITED. `schema_migrations` keys on the filename, so a file that
-- has already run never runs again.

DELETE FROM crewlet_event_parties;

INSERT INTO crewlet_event_parties (party, event_time, event_id)
SELECT agent_id, event_time, event_id FROM crewlet_events WHERE agent_id <> ''
ON CONFLICT (party, event_time, event_id) DO NOTHING;
