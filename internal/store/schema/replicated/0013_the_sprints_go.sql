-- Sprints leave the native tracker: the two objects, the stay table, the size
-- history that only sprint figures valued from, and the columns each of them
-- hung off.
--
-- # What went, and why it went whole
--
-- A sprint was a per-project object with a one-way state machine (`future` →
-- `active` → `closed`), a duty that advanced it on calendar boundaries, a
-- rollover that carried the spillover, a report, a burndown and a per-seat
-- capacity a workload screen was read against. None of it is half-removable:
-- the figures are derived from the STAYS, the stays are derived from a task's
-- `sprint_number`, and the capacity lives on the policy. Keeping any one of
-- them would leave a table written on every apply, on every node, to answer no
-- question — which is exactly what 0010 removed.
--
-- `points` and `estimate_min` STAY. A size is a fact about a task and the
-- query grammar filters, sorts and totals on both; what leaves is the window
-- those numbers were summed over, not the numbers.
--
-- # `tracker_measure_spans` goes with them
--
-- 0011 added it for one stated reason — "every sprint figure is a statement
-- about a past instant and all of them were valued at the present one" — and
-- its only two readers were the burndown and the sprint report. With those
-- gone it is a delete-and-rewrite of a task's whole size history on every
-- apply that nothing selects from.
--
-- # `tracker_status_spans` STAYS, minus its sprint half
--
-- The table is recomputed from `tracker_history` and its own comment names
-- cycle time and lead time over a window — questions about a task's own life
-- rather than about a sprint's. What goes is the `sprint_number` column and
-- the two indexes that led with it (the burndown, the burnup, the cumulative
-- flow); `tracker_status_spans_group_idx` is untouched.
--
-- # The columns are DROPped rather than left in place
--
-- A column nothing writes is read as NULL by the next person who finds it, and
-- `sprint_measure` in particular carried a NOT NULL DEFAULT 'points' that
-- would have gone on describing a measure no reader resolves. Turso takes
-- `ALTER TABLE … DROP COLUMN` for a column no index, view or trigger names,
-- which is why the two status-span indexes are dropped first.
--
-- 0002 AND 0011 ARE NOT EDITED. `schema_migrations` keys on the filename, so a
-- file that has already run never runs again: a fresh database and an upgraded
-- one converge here, by the same route.

-- The indexes go with their tables; naming them is belt-and-braces for an
-- engine that would keep an index over a dropped table.
DROP INDEX IF EXISTS tracker_sprints_state_idx;
DROP INDEX IF EXISTS tracker_sprints_spillover_idx;
DROP INDEX IF EXISTS tracker_sprints_unsettled_idx;
DROP INDEX IF EXISTS tracker_sprints_active_idx;
DROP INDEX IF EXISTS tracker_task_sprints_sprint_idx;
DROP INDEX IF EXISTS tracker_measure_spans_open_idx;

DROP TABLE IF EXISTS tracker_task_sprints;
DROP TABLE IF EXISTS tracker_measure_spans;
DROP TABLE IF EXISTS tracker_sprints;

-- BEFORE THE COLUMN, because a column an index names cannot be dropped.
DROP INDEX IF EXISTS tracker_status_spans_sprint_idx;
DROP INDEX IF EXISTS tracker_status_spans_status_idx;

ALTER TABLE tracker_status_spans DROP COLUMN sprint_number;

ALTER TABLE tracker_tasks DROP COLUMN sprint_number;

ALTER TABLE tracker_projects DROP COLUMN sprint_policy_json;
ALTER TABLE tracker_projects DROP COLUMN sprint_next;
ALTER TABLE tracker_projects DROP COLUMN active_sprint;
ALTER TABLE tracker_projects DROP COLUMN sprint_measure;
