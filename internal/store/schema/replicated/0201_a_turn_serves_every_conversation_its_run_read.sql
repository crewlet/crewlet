-- A turn serves every conversation its run read a wake of: the executor
-- domain's served rows (internal/executordomain, record version 5; ADR-0073's
-- amendment "a turn serves every conversation its run read").
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_EXECUTOR_LOG stream by its one applier, as 0073 states for
-- executor_sessions. Every column is copied from an `end` record and the
-- position it applied at.
--
-- # One row per conversation a turn served beyond its own
--
-- A run reads a wake of another conversation when it matched by work item (a
-- comment on the ticket the run works, written in the ticket's thread), and
-- the session the turn kept holds that context. So that conversation's next
-- turn continues the turn's session: its head read unions these rows with the
-- conversation's own, joined to the turn's one row in executor_sessions.
--
-- A SIDE TABLE, NOT A WIDER KEY: a turn still has exactly one row, which names
-- its one object, so the collector, the backup, the seat's byte budget and the
-- retention count the object once however many conversations read it, and
-- every statement that reads one turn by (agent_handle, turn_id) stays a point
-- read. No column here names an object.
--
-- # Its place in the conversation's line
--
-- `end_seq` is the position the turn's end applied at, which is also where it
-- reads the conversation: a turn of the conversation's own that the wake
-- would have run is dispatched once the seat frees, and reads at or after the
-- end. So the row outranks every turn of the conversation that read before
-- the end, and a turn that read after it (and so continued it) outranks the
-- row by its later end.
--
-- # How long a row lives
--
-- Until a later end of its conversation absorbs it (its read handed the turn
-- out), an end there retires the session it names, the retention deletes its
-- turn, or its seat is removed. While a row stands, the turn's own row is
-- never absorbed or deduped by its own conversation, which keeps the object
-- for the conversation that still reads it. The seat's budget may still
-- release the object; the row then tells how the turn ended, as any does.

CREATE TABLE executor_session_serves (
    agent_handle     TEXT    NOT NULL,
    turn_id          TEXT    NOT NULL,
    conversation_key TEXT    NOT NULL,
    end_seq          INTEGER NOT NULL,
    PRIMARY KEY (agent_handle, turn_id, conversation_key)
);
CREATE INDEX executor_session_serves_line_idx ON executor_session_serves (agent_handle, conversation_key, end_seq); -- a conversation's served head and endings
