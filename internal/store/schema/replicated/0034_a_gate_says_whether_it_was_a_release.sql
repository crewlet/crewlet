-- A node's gate row says whether it was an EVICTION or the node's own RELEASE.
--
-- # What a release is
--
-- When a node leaves a partition of the replicated estate it publishes, on
-- each identity-claiming log of that partition, a record releasing the log:
-- its own statement that nothing it publishes there afterwards applies on any
-- node. The applier records it exactly where an eviction is recorded, because
-- it is the same gate — every record the node wrote above it is dropped on
-- every holder, and a readmission lifts it — and the trim counts the two alike.
--
-- # Why the row has to say which
--
-- They differ in who said so and in what a caller is told. An eviction is an
-- operator's judgement of a machine that is not coming back; a release is the
-- node's own, and its readmission is its next join of the partition rather
-- than an operator's gesture. A write the gate dropped is refused `evicted` or
-- `released` accordingly, and the node's own write fence refuses only the
-- first: a node that left a partition is not a node the fleet removed.
--
-- # Why every existing row is an eviction
--
-- Nothing published a release before this column existed — the record is
-- written at a record version no earlier build reads (the tracker's 5 and the
-- knowledge base's 3), so an earlier build halts at one rather than applying
-- it. Every row already in either table was written by an eviction, which is
-- what the default says, and a row can only ever hold one of the two words.
ALTER TABLE tracker_evictions ADD COLUMN kind TEXT NOT NULL DEFAULT 'eviction'
    CHECK (kind IN ('eviction', 'release'));
ALTER TABLE pages_evictions ADD COLUMN kind TEXT NOT NULL DEFAULT 'eviction'
    CHECK (kind IN ('eviction', 'release'));
