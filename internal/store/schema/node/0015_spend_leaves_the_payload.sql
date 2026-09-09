-- The spend rollup's numbers become columns.
--
-- Every per-phase token count lived ONLY inside the event payload, so the
-- dashboard's spend breakdown selected `payload` — the phase's entire prompt
-- and its entire response, tens of kilobytes a row — hauled it across the
-- driver, and JSON-decoded it in Go to extract nine small values. A thousand
-- turns a day is three thousand of those rows a day.
--
-- That was the cheap half of the problem. The expensive half was a CAP: the
-- rollup took the newest 20 000 rows and silently dropped the rest, and the
-- code's own arithmetic said a busy org produces ~90 000 in the 30-day window
-- the dashboard offers. So the answer to "what did this company spend last
-- month" was short by however much of the window fell past the cap, with
-- nothing on the screen or in the response to say so — an undercount that
-- looks exactly like an underspend.
--
-- Promoted rather than left in the payload, because the payload is the only
-- place these values were and a rollup cannot index into a JSON blob. With
-- them as columns the query reads nine narrow values per row instead of a
-- multi-kilobyte document, the JSON decode disappears, and the cap goes with
-- it: the whole window is folded, which is what the number was always
-- supposed to mean.
--
-- Nine columns on a table where most rows are not phase completions. They
-- cost a byte each on those rows and no reader has to know they are there;
-- the alternative — a side table keyed on (event_time, event_id) — buys
-- nothing back and costs a second insert on the publish path, a join on the
-- read path, and a second retention sweep to keep them in step.

ALTER TABLE crewlet_events ADD COLUMN phase         TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN host_phase    TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN worker        TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN model         TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN turn_id       TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN iteration     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE crewlet_events ADD COLUMN input_tokens  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE crewlet_events ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE crewlet_events ADD COLUMN total_tokens  INTEGER NOT NULL DEFAULT 0;

-- BACKFILLED FROM THE PAYLOAD, so a deployment that upgrades keeps the spend
-- history it already recorded. The payload is still there and still holds
-- every one of these values; without this the breakdown would read zero for
-- everything written before this migration and an operator would watch a
-- month of spend vanish on an upgrade.
--
-- `model` falls back to `provider_key`, matching what the Go reader did: an
-- entry that names no model is identified by the provider slot it ran on.
UPDATE crewlet_events SET
    phase         = COALESCE(json_extract(payload, '$.phase'), ''),
    host_phase    = COALESCE(json_extract(payload, '$.host_phase'), ''),
    worker        = COALESCE(json_extract(payload, '$.worker'), ''),
    model         = COALESCE(json_extract(payload, '$.model'),
                             json_extract(payload, '$.provider_key'), ''),
    turn_id       = COALESCE(json_extract(payload, '$.turn_id'), ''),
    iteration     = COALESCE(json_extract(payload, '$.iteration'), 0),
    input_tokens  = COALESCE(json_extract(payload, '$.input_tokens'), 0),
    output_tokens = COALESCE(json_extract(payload, '$.output_tokens'), 0),
    total_tokens  = COALESCE(json_extract(payload, '$.total_tokens'), 0)
WHERE event_type = 'agent_phase_completed';
