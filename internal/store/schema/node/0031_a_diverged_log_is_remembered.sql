-- A log that diverged from this node's rows is remembered across a restart.
--
-- # What was missing
--
-- A broker restored from an older copy and written past a node's rows holds,
-- at that node's checkpoint, another record than the one the node consumed
-- there. The applier finds it by comparing the two broker instants and stops:
-- nothing past the checkpoint may be applied on top of rows the log does not
-- continue. That verdict lived only in memory, and the comparison that
-- re-derives it after a restart needs the log to still hold a record at the
-- checkpoint's sequence. Once that record is gone — the node was evicted and
-- the trim stopped counting it, the log was purged by hand, or the broker was
-- restored again from a copy without it — the restarted node had nothing to
-- compare, found its log replayable from its checkpoint, applied the other
-- history on top of its rows, and published that its log no longer diverged,
-- lifting the write fence on every peer.
--
-- # Why the node estate
--
-- The verdict is this node's own finding about its own rows: no log derives
-- it, so it is not replicated state, and a peer adopting this node's snapshot
-- has rows the log's record at their checkpoint agrees with. And an adoption
-- REPLACES the replicated file, so a row there would vanish exactly when it had
-- to be judged against the file that replaced it.
--
-- # What a row says
--
-- One row per stream: the checkpoint the verdict was reached at (generation
-- and sequence), the broker instant of the record that checkpoint names
-- (`consumed_at`) and of the record the log held there instead (`held_at`),
-- both in microseconds, and when this node found it. It holds exactly while
-- the checkpoint row names the same position and the same record — so a
-- reanchor, which moves the checkpoint into a new generation, and an adoption
-- that installs another checkpoint, are what end it, and nothing on the log
-- can.
CREATE TABLE statelog_diverged (
    stream      TEXT    NOT NULL PRIMARY KEY,
    generation  INTEGER NOT NULL,
    seq         INTEGER NOT NULL,
    consumed_at INTEGER NOT NULL,
    held_at     INTEGER NOT NULL,
    found_at    INTEGER NOT NULL
);
