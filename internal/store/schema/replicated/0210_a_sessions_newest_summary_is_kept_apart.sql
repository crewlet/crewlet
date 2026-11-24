-- A session's newest summary is kept apart (ADR-0073's amendment "A session's
-- newest summary is kept apart").
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_EXECUTOR_LOG stream by its one applier, as 0073 states for
-- executor_sessions: a turn's next run may launch on any node and start from it.
--
-- executor_session_summaries: the newest compaction summary a box read of a
-- turn kept, as the turn's end named it (internal/executorkeep's Summary), one
-- row beside the turn's executor_sessions row. It goes with that row: an
-- absorption or a dedupe deleting the row, the retention and the seat's
-- removal; a retirement and the seat's budget, which take the row's session and
-- count no summary, keep it. `object` is the column
-- internal/objstore/references declares (executordomain.SummaryObjectReferences),
-- so the collector keeps the sealed summary while a row names it. `kept_at` is
-- when the box read kept it (`store.EncodeTime`), `bytes` its whole size,
-- `kept_bytes` the bytes kept of it, `cut` whether those are its end, and
-- `version` the record's packed position, as every row of the domain keeps one.
CREATE TABLE executor_session_summaries (
    agent_handle TEXT    NOT NULL,
    turn_id      TEXT    NOT NULL,
    object       TEXT    NOT NULL,
    object_hash  TEXT    NOT NULL,
    object_size  INTEGER NOT NULL,
    content_key  TEXT    NOT NULL,
    harness      TEXT    NOT NULL,
    session_id   TEXT    NOT NULL DEFAULT '',
    kept_at      INTEGER NOT NULL,
    bytes        INTEGER NOT NULL,
    kept_bytes   INTEGER NOT NULL,
    cut          INTEGER NOT NULL DEFAULT 0,
    version      INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, turn_id)
);
CREATE INDEX executor_session_summaries_object_idx ON executor_session_summaries (object); -- the collector, the audit and the backup
