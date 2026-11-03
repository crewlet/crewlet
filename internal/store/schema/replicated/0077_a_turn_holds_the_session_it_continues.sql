-- A turn holds the session it continues: the executor domain's `begin` record
-- (internal/executordomain, record version 2; ADR-0073's amendment "a turn's
-- first read holds the session it was handed").
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_EXECUTOR_LOG stream by its one applier, as 0073 states for the rest
-- of executor_sessions. Every column below is copied from a record.
--
-- # The base a turn's first read froze
--
-- `base_turn` is the ended turn of the same conversation whose session the
-- turn's first launch read as the conversation's head and froze to continue,
-- written by the turn's `begin` where that turn's row was still there, ended and
-- naming the session the read saw; '' where the read named none, or the base had
-- gone by the time the record applied. It is set once and never moved.
--
-- # The hold
--
-- While the turn has kept no session of its own (`launch_seq` 0) and has not
-- ended, its row HOLDS that base: no release takes it — the seat budget's,
-- an end's absorption or work-key dedupe, the retention's delete — because the
-- turn's relaunch continues it. The hold lifts with the turn's first landed
-- capture or its end, both of which move the row out of the index below. Every
-- release path asks the index about the one row it is about to release, inside
-- the apply, rather than through a subquery of the statement that releases.
--
-- # What a capture says of its session
--
-- `context_tokens` is the whole input of the last request the session's harness
-- sent its model, as its transcript recorded it (0 where it recorded none), and
-- `interrupted` (0 or 1) that its harness was cut off rather than exiting
-- cleanly: what a launch continuing the session after its turn ended needs —
-- the first for its budget floor, the second for its brief — and which only
-- the run's record held before, deleted with the turn.

ALTER TABLE executor_sessions ADD COLUMN base_turn      TEXT    NOT NULL DEFAULT '';
ALTER TABLE executor_sessions ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE executor_sessions ADD COLUMN interrupted    INTEGER NOT NULL DEFAULT 0;

CREATE INDEX executor_sessions_held_idx ON executor_sessions (agent_handle, base_turn)
    WHERE end_seq IS NULL AND launch_seq = 0 AND base_turn <> '';  -- the bases live turns hold
