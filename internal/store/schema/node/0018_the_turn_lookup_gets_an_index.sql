-- Reading one turn's events stops scanning the log.
--
-- `turn_id` became a column in 0015, promoted out of the payload so the spend
-- rollup could fold it. Nothing indexed it: that migration's subject was the
-- rollup, which groups over the whole window and reads every row anyway, so an
-- index bought it nothing.
--
-- A trace does the opposite. "Show me this turn" selects the handful of rows
-- carrying one id out of thirty days of them, and without an index that is a
-- full scan of the largest table in the store — on the path a person waits on,
-- growing with the retention window rather than with the answer.
--
-- (event_time, event_id) trail the key because they are the sort: the same
-- descending order every listing here uses, so the index answers the ordering
-- too and the query never sorts what it read.
CREATE INDEX IF NOT EXISTS crewlet_events_turn_idx
    ON crewlet_events (turn_id, event_time, event_id);
