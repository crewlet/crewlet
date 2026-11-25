-- A session's summary names its place (ADR-0073's amendment "A session's newest
-- summary is kept apart").
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_EXECUTOR_LOG stream by its one applier, as 0212 states for the rest of
-- executor_session_summaries.
--
-- `launch_seq` and `capture_seq` are the place of the capture whose stamp first
-- named the summary (internal/executorkeep's Summary.Place), the take of the box
-- read that found it: a session captured at or after it holds the compaction it
-- tells, one captured before does not, so a reader tells by it whether the
-- summary is newer than the capture a run continues. Every end that names a
-- summary names its place, each from 1; the DEFAULT is what ADD COLUMN needs.
ALTER TABLE executor_session_summaries ADD COLUMN launch_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE executor_session_summaries ADD COLUMN capture_seq INTEGER NOT NULL DEFAULT 0;
