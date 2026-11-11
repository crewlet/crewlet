-- The org chart's seat order is replicated.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_TRACKER_LOG stream by its one applier. The roster is reconciled from
-- the chart by the chart apply (worklet.Writer.ApplyChart), as a project's
-- chart-owned fields are, and its order is decided by the writer, so every
-- node holds the same places whatever its own build would collate.
--
-- tracker_rosters is the roster object's own row: the chart's handles in the
-- order of their seats' names, stamped with the activation they came from.
CREATE TABLE tracker_rosters (
    name        TEXT    NOT NULL PRIMARY KEY,
    chart_epoch INTEGER NOT NULL,
    version     INTEGER NOT NULL,
    document    BLOB    NOT NULL
);

-- tracker_seat_places is each seat's place in that order, rewritten whole by
-- the roster's apply. `sort=assignee` joins it on its key, one seek a task:
-- the order spelled into the statement as a CASE naming every seat cost each
-- task a walk of the chart, and a list bound per read is sought or scanned at
-- the planner's guess.
CREATE TABLE tracker_seat_places (
    handle TEXT    NOT NULL PRIMARY KEY,
    place  INTEGER NOT NULL
);
