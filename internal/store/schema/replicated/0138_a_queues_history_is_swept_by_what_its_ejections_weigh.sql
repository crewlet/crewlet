-- A queue's history is swept by what its ejections weigh.
--
-- A repository's ended queue entries past gitlet.QueueHistory are removed oldest
-- first — at most gitlet.queueHorizonRows by each candidate, eject and land
-- record, and at most gitlet.QueueSweepRows by a queue_sweep record of the
-- retention duty — and 0110 bounded both by a COUNT of rows alone. An
-- ejected entry is not a narrow row: its `ejected` column holds the JSON of
-- what a person reads first — the failed checks, and up to
-- gitlet.MaxEjectedPaths conflicted paths within gitlet.MaxConflictBytes — about
-- 16 KiB at its widest, and deleting it frees its overflow pages. A sweep of
-- 3 999 of them was some 64 MiB, past the third of
-- statelog.ApplyTxTimeBudget every other bound of the domain's deletes is
-- sized to (gitlet.PurgePartBytes), in one apply on every data node.
--
-- code_queue gains ejected_bytes: what `ejected` takes, in bytes, written by
-- the one apply that writes `ejected` (an eject's), 0 on every other entry.
-- THE BACKFILL computes it for the rows an eject already wrote, so this
-- copy is the one the log's records make when applied afresh.
--
-- code_queue_ended_idx is created again, its key the ended entries' WHOLE
-- order — when each ended, then its id — and ejected_bytes beside it:
--   - the sweep walks the entries past the history in that order, weighing
--     each by ejected_bytes from the index alone, so weighing an ejection
--     never reads its overflow pages, and stops at a row count or at a byte
--     budget, whichever comes first;
--   - the walk names its LAST ENTRY BY ITS KEY, and the apply removes the
--     entries up to that key — a range of the index, never "the first n by
--     when they ended", which at a tie (a landing ends up to 64 entries at one
--     instant) would remove whichever the engine's row order put first, an
--     order a copy of the file need not share.
--
-- Class: code_queue stays REPRODUCIBLE: a column the applier writes from the
-- records on a repository's own subject, and an index, which adds no row.

ALTER TABLE code_queue ADD COLUMN ejected_bytes INTEGER NOT NULL DEFAULT 0;

UPDATE code_queue SET ejected_bytes = length(CAST(ejected AS BLOB)) WHERE ejected != '';

DROP INDEX IF EXISTS code_queue_ended_idx;

CREATE INDEX code_queue_ended_idx ON code_queue (repo_id, ended_at, entry_id, ejected_bytes)
    WHERE state IN ('landed', 'ejected', 'dequeued');    -- a repository's ended entries in their order, weighed: the queue's horizon, its sweep, its merge rate
