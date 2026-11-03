-- The executor domain lands: the state log's tenth domain, the record of each
-- seat's executor sessions. internal/executordomain is the domain (ADR-0073).
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_EXECUTOR_LOG stream by one deterministic applier. A seat's next turn
-- on a conversation may launch on any node, in any box, and continue the
-- session its previous turn left, so the name of that session is the whole
-- fleet's.
--
-- ## A row names an object; the bytes are in the object store
--
-- One row per (seat handle, turn) whose executor captured its harness's
-- session, naming the sealed object in the fleet's object store that holds it
-- (ADR-0026). `object` is the column internal/objstore/references declares,
-- so the collector keeps every object a row names and deletes it a day after
-- the row stops naming it. A row naming no object (NULL) is an ending alone:
-- a turn that captured nothing, or one whose session the seat's byte budget
-- released.
--
-- ## The rules every replicated table keeps
--
-- NO FOREIGN KEY. NO `UNIQUE` OUTSIDE A PRIMARY KEY: a constraint violation
-- inside the apply transaction aborts it on every node at once. Every instant
-- a row records of its own is the broker's store timestamp of the record that
-- wrote it, as `store.EncodeTime` integers, and a version or a position is a
-- record's packed position `(generation << 40) | sequence`.

-- executor_sessions: one row per turn of a seat that captured a session or
-- ended. `ending` is the end record's own account (internal/executordomain's
-- Ending) as the record carried it, with `outcome` and `delivered` beside it
-- as columns: the outcome to list by, and `delivered` because a later attempt
-- at the same unit of work merges into it. `last_at` is the row's own last
-- instant (its end's, else its newest capture's), what the retention measures.
-- `read_through` and `absorbs` are what the turn's first launch read of its
-- conversation: the position, and the turns that read handed out in full,
-- which are what the turn's end may absorb. `first_seq` is the position of the
-- record that first wrote the row, set once and never moved, which is what a
-- seat's sessions are paged by: every other position here moves with a
-- capture, an end or a release.
CREATE TABLE executor_sessions (
    agent_handle     TEXT    NOT NULL,
    turn_id          TEXT    NOT NULL,
    conversation_key TEXT    NOT NULL DEFAULT '',
    work_key         TEXT    NOT NULL DEFAULT '',
    harness          TEXT    NOT NULL DEFAULT '',
    harness_version  TEXT    NOT NULL DEFAULT '',
    session_id       TEXT    NOT NULL DEFAULT '',
    object           TEXT,
    object_hash      TEXT    NOT NULL DEFAULT '',
    object_size      INTEGER NOT NULL DEFAULT 0,
    content_key      TEXT    NOT NULL DEFAULT '',
    archive_format   INTEGER NOT NULL DEFAULT 0,
    launch_seq       INTEGER NOT NULL DEFAULT 0,
    capture_seq      INTEGER NOT NULL DEFAULT 0,
    home             TEXT    NOT NULL DEFAULT '',
    workspace        TEXT    NOT NULL DEFAULT '',
    captured_at      INTEGER NOT NULL DEFAULT 0,
    read_through     INTEGER NOT NULL DEFAULT 0,
    absorbs          TEXT    NOT NULL DEFAULT '',
    ended_at         INTEGER,
    end_seq          INTEGER,
    outcome          TEXT    NOT NULL DEFAULT '',
    delivered        INTEGER NOT NULL DEFAULT 0,
    ending           TEXT    NOT NULL DEFAULT '',
    ask_excerpt      TEXT    NOT NULL DEFAULT '',
    last_at          INTEGER NOT NULL DEFAULT 0,
    first_seq        INTEGER NOT NULL,
    version          INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, turn_id)
);
CREATE INDEX executor_sessions_object_idx ON executor_sessions (object);                                  -- the collector, the audit and the backup
CREATE INDEX executor_sessions_conversation_idx ON executor_sessions (agent_handle, conversation_key, end_seq); -- a conversation's head, its endings and its dedupe
CREATE INDEX executor_sessions_ended_idx ON executor_sessions (agent_handle, end_seq);                  -- the seat budget's release, oldest end first
CREATE INDEX executor_sessions_seat_idx ON executor_sessions (agent_handle, first_seq);                 -- a seat's sessions, newest first
CREATE INDEX executor_sessions_expiry_idx ON executor_sessions (last_at, agent_handle, turn_id);       -- the retention duty's nomination
CREATE INDEX executor_sessions_unended_idx ON executor_sessions (last_at, agent_handle, turn_id) WHERE end_seq IS NULL; -- the retention duty's lost ends

-- executor_session_tombs: a turn that takes no further record: ended by its
-- own `end`, or live when its seat was removed. It outlives the row an
-- absorption or a seat's removal deletes, so a capture decided before either
-- recreates nothing, and goes with the retention, which deletes the row and
-- its tombstone together.
CREATE TABLE executor_session_tombs (
    agent_handle     TEXT    NOT NULL,
    turn_id          TEXT    NOT NULL,
    conversation_key TEXT    NOT NULL DEFAULT '',
    at_seq           INTEGER NOT NULL,
    at               INTEGER NOT NULL,
    reason           TEXT    NOT NULL,
    PRIMARY KEY (agent_handle, turn_id)
);
CREATE INDEX executor_session_tombs_at_idx ON executor_session_tombs (at, agent_handle, turn_id); -- the retention duty's nomination

-- executor_seat_removals: the position a seat's rows were last removed at, by
-- the mailbox retirement. A capture or an end carries the removal its decide
-- saw, and one decided before a removal it did not see writes nothing when it
-- lands after it: so a turn that had no row when its seat was removed, and so
-- no tombstone, is not recreated for a seat that is gone. It goes with the
-- retention, past which no record decided before it can still land.
CREATE TABLE executor_seat_removals (
    agent_handle TEXT    NOT NULL PRIMARY KEY,
    at_seq       INTEGER NOT NULL,
    at           INTEGER NOT NULL
);
CREATE INDEX executor_seat_removals_at_idx ON executor_seat_removals (at, agent_handle); -- the retention duty's nomination

-- executor_evictions: a node's removal from THIS log, or its readmission.
CREATE TABLE executor_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- executor_log_generations: every reanchor's audit row.
CREATE TABLE executor_log_generations (
    generation            INTEGER NOT NULL PRIMARY KEY,
    at                    INTEGER NOT NULL,
    by                    TEXT    NOT NULL DEFAULT '',
    new_stream_created_at INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen    INTEGER NOT NULL DEFAULT 0,
    record_id             TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- The log's own machinery, whose statements the framework generates (0001 and
-- 0021).
-- ---------------------------------------------------------------------------

-- executor_ops: what this node's applier has written. It TRAVELS in every
-- donated snapshot and stays out of the identity claim.
CREATE TABLE executor_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL,
    stored_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX executor_ops_swept_idx ON executor_ops (applied_at); -- the ops retention sweep

-- executor_log_deferred: a record this build could not decode, retained byte
-- for byte at its original position.
CREATE TABLE executor_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    version      INTEGER NOT NULL,
    payload      BLOB    NOT NULL,
    stored_at    INTEGER NOT NULL
);
CREATE INDEX executor_log_deferred_subject_idx ON executor_log_deferred (subject_id, subject_kind); -- the applier's per-object deferral probe

-- One row per scope path of a deferred record.
CREATE TABLE executor_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX executor_log_deferred_scope_idx ON executor_log_deferred_scope (path, position); -- the read barrier's coverage probe and the writer's step 0
