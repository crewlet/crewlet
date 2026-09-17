-- A task's SIZE gets a history, because every sprint figure is a statement
-- about a past instant and all of them were valued at the present one.
--
-- # What was wrong
--
-- `tracker_task_sprints` is one row per STAY and every sprint figure is a
-- predicate over the from/to pair — but the VALUE each predicate summed was
-- `t.points` or `t.estimate_min`, the task's current row. So re-estimating a
-- task from 3 to 8 on day 5 of a sprint moved what day 1 had already reported:
-- `committed` rose by five points nobody committed, the burndown's first point
-- rose with it, and the chart of a finished sprint changed shape whenever
-- somebody tidied an estimate afterwards.
--
-- The burndown was the worst of it. `burndownSeries` walks the sprint's
-- instants and sums each member's measure at each one, and it read that
-- measure ONCE per task before the walk — so a chart whose whole subject is
-- change in time was drawn with a value that had none. A sprint that
-- delivered exactly what it took on rendered as one that had been handed more
-- work and finished it.
--
-- # Why a table rather than the history rows
--
-- `tracker_status_spans` is recomputed from `tracker_history`, and the obvious
-- symmetry would be to do the same here. It cannot be: a history row's
-- `fields_json` carries what a PERSON reads — `points` as `"8"` and `estimate`
-- as `"90m"`, with an empty string for unset — and a number parsed back out of
-- display text is a number this engine did not store. Worse, `MaxDeltas`
-- trims a record carrying more than thirty-two field changes, deterministically
-- but by field NAME, so a large patch can drop the points delta entirely. A
-- figure cannot rest on a row that is allowed to omit it.
--
-- So the spans are derived on the task DOCUMENT, exactly as the sprint stays
-- are and for the same reason their own comment gives: the applier holds the
-- before and the after, and the instant comes from the broker rather than from
-- a writer's clock. These rows are that document exploded, rebuilt wholesale
-- on every apply like every other collection, so a reprocess converges.
--
-- # Both measures on one row
--
-- A project's measure is `points` or `estimate_min` and a company may change
-- which. One row carrying both means the span boundaries are the same set
-- whichever measure is read, so switching a project's measure re-reads the
-- same history rather than revealing a differently-shaped one — and a task
-- whose points moved while its estimate did not still has one span per
-- change, which is what makes "the measure at instant T" a single lookup.
--
-- # Swept with the task
--
-- No retention of its own: the spans belong to a task and go when it is
-- purged, so the purge deletes them with its other child rows and nothing in
-- `maintenance`'s job list needs to know about them. The table joins
-- `tracker.ReproducibleTables`, which is what puts it inside the domain's
-- identity claim and out of every donated snapshot's machinery. The per-task
-- cap is `MaxMeasureSpans`, applied on the document where the sprint stays'
-- cap is applied.
CREATE TABLE tracker_measure_spans (
    task_id      TEXT    NOT NULL,
    from_at      INTEGER NOT NULL,
    to_at        INTEGER,
    points       REAL    NOT NULL DEFAULT 0,
    estimate_min INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (task_id, from_at)
);

-- THE LOOKUP IS "this task, at or before this instant, newest first", which is
-- what every reader asks: the primary key already orders `from_at` within a
-- task, so the seek is a descending range scan on it and this index would be
-- the same columns. Declared anyway for `to_at IS NULL` — the OPEN span, which
-- is the task's measure now and the one `remaining` reads — because that is a
-- predicate the key cannot narrow.
CREATE INDEX tracker_measure_spans_open_idx ON tracker_measure_spans (task_id, to_at);
