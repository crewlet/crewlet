-- Who answered a question becomes a column of the event log, so a person's
-- answers are a range seek rather than a scan of every answer in the window.
--
-- WHAT A COLUMN BUYS HERE is the question `answered_decisions` asks: "the
-- coding runs' questions THIS person answered in the last seven days". The
-- record of an answer (sandbox_run_answered) names who gave it in its
-- `answered_by` field, which store.ExtractTags already lifts into the row's
-- tags; read through the tags, the question paged through every answer every
-- person gave in the window and kept one person's in memory.
--
-- WHO HAS TO AGREE ON IT: this node alone. It is a column of the node's own
-- audit log, derived by the writer from the event it stores (the same tag),
-- exactly as channel_id and work_item are, so the column and the tag can
-- never name two different people.
--
-- An empty string, never NULL, for a row nobody answered: the shape every
-- promoted column has (0015, 0029, 0033, 0034), so a reader compares against
-- '' and the partial index below leaves those rows out.
--
-- NOT BACKFILLED. The field was first written by a build no release has
-- shipped, and nothing an unreleased build wrote is read.
ALTER TABLE crewlet_events ADD COLUMN answered_by TEXT NOT NULL DEFAULT '';

-- A PERSON'S ANSWERS ARE A RANGE SEEK: (answered_by, event_time) is the order
-- the answered list reads them in, newest first. PARTIAL over the rows that
-- name somebody, because almost no row in the log is an answer.
CREATE INDEX IF NOT EXISTS crewlet_events_answered_by_idx
    ON crewlet_events (answered_by, event_time, event_id)
    WHERE answered_by <> '';
