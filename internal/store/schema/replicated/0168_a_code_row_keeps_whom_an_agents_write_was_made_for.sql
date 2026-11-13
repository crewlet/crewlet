-- A code row keeps whom an agent's write was made for.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_CODE_LOG stream by its one applier, as the rest of these tables.
--
-- `working_for` is the carrier an agent's record rode (ADR-0109, code record
-- version 2): a revision's push or open, a push row's push, a queue entry's
-- enqueue. It is held as the carrier's JSON, and NULL on every other row: a
-- record that carries none, every version-1 record, and every row applied
-- before this migration ran. A landing's push rows take theirs from the
-- entries' rows in the landing's own apply, which is the one reader at first;
-- the engine's later records (a revision's facts, a failed check of a head,
-- a landing the home found, a queue's landing) and the queue's close of a
-- landed change's item read them back from crewlet#798's piece 4c-ii on. It
-- lands in the build that first writes the carrier, before most of what
-- reads it, since a replicated migration runs on each node when it opens and
-- not at a log position: a column a later build added and filled from
-- version-2 records would hold NULL for the records a node applied before it
-- upgraded and the carrier for the same records on a node that applied them
-- after, for good.
ALTER TABLE code_revisions ADD COLUMN working_for TEXT;
ALTER TABLE code_pushes ADD COLUMN working_for TEXT;
ALTER TABLE code_queue ADD COLUMN working_for TEXT;
