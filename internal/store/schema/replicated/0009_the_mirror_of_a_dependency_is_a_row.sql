-- A dependency's MIRROR becomes a row, so "one-sided" is a fact a query can
-- seek rather than a flag nobody could compute.
--
-- # What was missing
--
-- `waiting_on` is authored on the dependent and mirrored onto the blocker as
-- an entry in its own `Dependents` list. 0002 shipped `tracker_relations`
-- with `one_sided`/`one_sided_final` and a partial index annotated "the
-- relations repair duty's selection" — but the mirror lived only inside the
-- blocker's `document` JSON, so nothing could answer "does the blocker list
-- this dependent" without decoding a document per edge. The flag therefore
-- had no producer at all: it was written through from whatever the record
-- said, and no record ever said anything.
--
-- # Why a table rather than a JSON test
--
-- Because the applier computes this on every task commit that touches either
-- end, on every node, for ever. An exploded child table makes it one indexed
-- EXISTS against rows the same transaction has already written; the
-- alternative — a `json_extract` walk over the blocker's document — is a read
-- of a 64 KiB blob per edge, and its cost grows with the blocker's BODY.
--
-- It is the same shape as every other collection the applier explodes
-- (`tracker_watchers`, `tracker_relations`, `tracker_task_tags`): deleted and
-- rewritten from the record's own collection, so a reprocess converges rather
-- than accumulating.
--
-- # The two directions, and why both indexes exist
--
-- `(task_id, dependent_id)` is "who waits on this blocker", which is what a
-- close reads to say who it unblocks and what `get_work_item` renders. The
-- reverse index is the REPAIR's: given a dependent's authored edge, does the
-- blocker list it — a probe keyed on the dependent.
--
-- 0002 IS NOT EDITED. `schema_migrations` keys on the filename, so a file that
-- has already run never runs again: a fresh database and an upgraded one
-- converge here, by the same route.

CREATE TABLE tracker_task_dependents (
    -- The BLOCKER, whose own commit wrote this row.
    task_id      TEXT NOT NULL,
    -- The task that waits on it.
    dependent_id TEXT NOT NULL,
    PRIMARY KEY (task_id, dependent_id)
);
CREATE INDEX tracker_task_dependents_rev_idx ON tracker_task_dependents (dependent_id, task_id);  -- the repair's probe: does my blocker list me

-- And the EDGE gets its own instant, because that is what the repair ages on.
--
-- The repair waits [OneSidedRepairAge] before writing a mirror, so it never
-- races a gesture that is still running — and the only column it could have
-- aged on was the dependent task's `updated_at`, which any unrelated edit
-- resets. A busy task would postpone its own edge's repair indefinitely, and
-- the busiest tasks are exactly the ones that acquire dependencies.
--
-- Written from the relation's own authored instant, which the writer stamps
-- and the record carries — so every node writes the same value and the
-- selection is identical on all of them. An edge that carries none is aged as
-- very old, which repairs it promptly: an edge nobody can date is one the
-- duty should not be waiting on.
ALTER TABLE tracker_relations ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
