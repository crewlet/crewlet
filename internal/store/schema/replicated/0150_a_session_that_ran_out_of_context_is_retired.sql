-- A session that ran out of context is retired (ADR-0073's amendment of that name).
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_EXECUTOR_LOG stream by its one applier, as 0073 states for the rest of
-- executor_sessions.
--
-- `retired_by` is the turn whose end took this row's session away, '' for none. The
-- object goes in the same write and session_id stays, so a reader can say which
-- session went and why rather than taking the row for a budget's release.
ALTER TABLE executor_sessions ADD COLUMN retired_by TEXT NOT NULL DEFAULT '';
