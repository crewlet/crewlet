-- A session keeps its large media apart (internal/executordomain, record
-- version 9; ADR-0073's amendment "a session's large media is kept apart").
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_EXECUTOR_LOG stream by its one applier, as 0073 states for
-- executor_sessions: a turn's next launch may restore the session on any node.
-- Every column is copied from a `capture` record and the position it applied at.
--
-- executor_session_media: one row per media object a turn's row names, each a
-- gzip member of the session's archive that the session's text object frames
-- by its digest and a restore splices back. `digest` is the member's SHA-256,
-- lowercase hex; `object`, `object_hash`, `object_size` and `content_key` are
-- the sealed object holding it, `object` the column internal/objstore/references
-- declares (executordomain.SessionMediaReferences), so the collector keeps it
-- while a row names it. `version` is the record's packed position.
--
-- REACHABILITY, NOT A COUNT: a turn's capture names its whole media set, and
-- may carry an object its turn's own rows or the base its row holds already
-- name, so one object may have several rows. The rows go with their turn's
-- object, wherever a record takes that object off the turn's row or deletes
-- the row, and the collector deletes an object once no row names it.
CREATE TABLE executor_session_media (
    agent_handle TEXT    NOT NULL,
    turn_id      TEXT    NOT NULL,
    digest       TEXT    NOT NULL,
    object       TEXT    NOT NULL,
    object_hash  TEXT    NOT NULL,
    object_size  INTEGER NOT NULL,
    content_key  TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, turn_id, digest)
);
CREATE INDEX executor_session_media_object_idx ON executor_session_media (object); -- the collector, the audit and the backup
