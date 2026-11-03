-- A schedule's history is read from the usage domain (0023), every data
-- node's, rather than from the dispatch ledger of whichever node answers: that
-- ledger is the node's own (`scheduled_runs`), swept after a week, and records
-- `fired` when the scheduler sends a fire, before the seat's node decides
-- whether to run it at all. What the history needs is here.
--
-- # The tick a fire stands for
--
-- `scheduled_at` is the instant the fire was due and `fire_label` that tick as
-- the ledger keys it (the schedule's local wall-clock minute, YYYYmmddTHHMM).
-- On a catch-up they differ from `fired_at`, and the tick being made up is the
-- fact a reader is after. Carried by a schedule record from version 3
-- (internal/usage), written only by the usage domain's applier.

ALTER TABLE usage_schedule_runs ADD COLUMN scheduled_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_schedule_runs ADD COLUMN fire_label   TEXT    NOT NULL DEFAULT '';

-- # A page of fires is read in the order it is shown
--
-- The history pages fires newest first, ordered by `fired_at` and then the
-- rest of the row's identity (node, scope, name, target), so two fires at one
-- instant page the same way on every read. `usage_schedule_runs_fired_idx`
-- (0029) held `fired_at` alone, which this engine's planner does not use for a
-- longer ORDER BY: the company's first page walked every fire of the 181-day
-- horizon and sorted them, on every poll of the Schedules page. An index over
-- the whole order is walked from its newest end and stopped at the page.
--
-- And ONE schedule's own history (`schedule_runs`) seeks that schedule and
-- reads its fires in the same order: through the index above, a weekly
-- schedule beside twenty hourly ones walked the whole horizon to fill a page.

DROP INDEX usage_schedule_runs_fired_idx;
CREATE INDEX usage_schedule_runs_newest_idx
    ON usage_schedule_runs (fired_at, node, scope_type, scope_id, name, target);  -- a page of every schedule's fires
CREATE INDEX usage_schedule_runs_schedule_idx
    ON usage_schedule_runs (scope_type, scope_id, name, fired_at, node, target);  -- a page of one schedule's fires

-- # What became of a fire
--
-- usage_schedule_superseded: one row per fire of a schedule that a seat did
-- not run, because a later fire of the same schedule reached it at the same
-- time and ran in its place, named by the TRACE the fire was sent under.
--
-- Who has to agree on it: every data node, and only the current value, the
-- usage domain's answer (adr/0020). Published by the node whose SEAT set the
-- fire aside, in its own schedule-day for the day it recorded that, which is
-- neither the node whose scheduler sent the fire nor, when a seat waited past
-- midnight, the fire's day. Derived on the scheduler's node instead, the fact
-- was found only where that node also ran the seat, and only while the fire's
-- own day was still being derived. The history joins the two tables by trace
-- (usage.ScheduleRuns), so a fire reads as skipped wherever either half was
-- published.
--
-- `version` is on every row for `usage_schedule_runs`' reason: a schedule
-- record has no head row, so its guard is the newest version among its rows in
-- both tables. Divergent, like every row table of this domain; the horizon is
-- the applier's, by the record's day.
CREATE TABLE usage_schedule_superseded (
    day        TEXT    NOT NULL,
    node       TEXT    NOT NULL,
    scope_type TEXT    NOT NULL,
    scope_id   TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    trace_id   TEXT    NOT NULL,
    at         INTEGER NOT NULL,
    version    INTEGER NOT NULL,
    PRIMARY KEY (day, node, scope_type, scope_id, name, trace_id)
);

-- The history's join: is this fire's trace one some node set aside.
CREATE INDEX usage_schedule_superseded_trace_idx ON usage_schedule_superseded (trace_id);
