-- A spend record's price, its per-model split and its unreported mark become
-- columns — `cost_usd`, `models` and `run_spend_unreported` — beside the
-- identity and token counts 0015 promoted.
--
-- # Why columns
--
-- The reads that fold spend records need these from every record they read:
-- the rollup over a window and the live projection's startup seed all three,
-- and the turn list the split, since a turn's models include the ones only a
-- split names. A value inside `payload` is reached only by parsing that
-- payload, and on a phase record the payload is the phase's prompts, its
-- response and its tool log. A column is read without a parse.
--
-- # What each holds
--
--   * `cost_usd` is the payload's `cost_usd` when that is a JSON number, and 0
--     otherwise.
--   * `models` is the payload's `models` when that is a JSON array, and `[]`
--     otherwise. The empty array is the one spelling of "no split", and the
--     spend reads count such a record whole under its `model`. Every value
--     either writer puts here is a JSON array, which is what lets the turn
--     list expand the column with json_each on every row it reads.
--   * `run_spend_unreported` is 1 when the payload's `run_spend_unreported` is
--     JSON true, and 0 otherwise.
--
-- The writer derives the same three from every spend record it appends
-- (internal/store's spendOf), the price and the mark by the same rules. For
-- the split it stores what internal/tokens' DecodeModels reads out of the
-- payload's list, re-encoded, and `[]` where that rule refuses the list; the
-- backfill below copies any array as the payload holds it.
--
-- # The backfill
--
-- Over both spend types, from each row's own payload, so the history a node
-- already holds reads as the rows written after it do. A row whose payload is
-- not valid JSON is left out and keeps the defaults, which is what the writer
-- stores for a spend record whose payload does not decode. A row of any other
-- type keeps them too, as the writer leaves them on one.
--
-- ONLY A ROW THAT CARRIES ONE OF THE THREE IS REWRITTEN. Updating a row
-- writes the whole row again, payload and all, and inside the migration's one
-- transaction every page it writes stays in the write-ahead log until the
-- next checkpoint. A row whose price is zero, whose split is empty or absent
-- and whose mark is unset has nothing to write, and every phase record a
-- build that predates the split wrote, quoting no price, is such a row. The
-- test for it parses the payload once more on a row that is written.
--
-- WHAT IT COSTS, measured on a development container over 10 000 phase
-- records carrying 33 KiB payloads: about 12 s where every record carries a
-- split, with the write-ahead log growing by about twice the size of the rows
-- rewritten; about 4 s where none carries anything, with no growth at all. So
-- a node whose log holds many records with a split needs free disk of about
-- twice their size to boot through this migration.
--
-- ONE STATEMENT, UNBATCHED, for the reason 0029 gives: a migration runs inside
-- Open, before the node serves anything, so the rows it rewrites starve no
-- live append, and what it costs is boot time on a large log, paid once.
ALTER TABLE crewlet_events ADD COLUMN cost_usd             REAL    NOT NULL DEFAULT 0;
ALTER TABLE crewlet_events ADD COLUMN models               TEXT    NOT NULL DEFAULT '[]';
ALTER TABLE crewlet_events ADD COLUMN run_spend_unreported INTEGER NOT NULL DEFAULT 0;

UPDATE crewlet_events SET
    cost_usd = CASE json_type(payload, '$.cost_usd')
                   WHEN 'integer' THEN json_extract(payload, '$.cost_usd')
                   WHEN 'real'    THEN json_extract(payload, '$.cost_usd')
                   ELSE 0 END,
    models = CASE json_type(payload, '$.models')
                 WHEN 'array' THEN json_extract(payload, '$.models')
                 ELSE '[]' END,
    run_spend_unreported = CASE json_type(payload, '$.run_spend_unreported')
                               WHEN 'true' THEN 1
                               ELSE 0 END
WHERE event_type IN ('agent_phase_completed', 'auxiliary_call_completed')
  -- A CASE, whose branch is evaluated only when json_valid holds, rather
  -- than terms AND-ed beside it, whose order of evaluation is the planner's:
  -- internal/learning's retiredExemplars records an AND beside json_valid
  -- that let the call after it parse a value that was not JSON, and here one
  -- such row would fail the migration and the boot with it.
  AND CASE WHEN json_valid(payload) THEN
          json_type(payload, '$.cost_usd') IN ('integer', 'real')
              AND json_extract(payload, '$.cost_usd') <> 0
          OR json_type(payload, '$.models') = 'array'
              AND json_extract(payload, '$.models') <> '[]'
          OR json_type(payload, '$.run_spend_unreported') = 'true'
      ELSE 0 END;
