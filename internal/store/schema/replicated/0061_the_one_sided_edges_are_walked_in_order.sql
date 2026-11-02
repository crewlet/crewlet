-- The one-sided edges are walked in the order the repair selects them.
--
-- # tracker_relations_repair_idx
--
-- The one-sided repair duty selects the dependency edges whose mirror was
-- never written a batch at a time, ordered by the dependent and then the
-- blocker (`ScanOneSided` in internal/worklet/onesided.go), through the
-- partial index replicated 0002 shipped for exactly that predicate. The index
-- held the dependent alone, so the order's second column was not in it:
-- measured on the tracker's plan fixture, the selection walked every
-- one-sided edge in the company through the index and sorted them all to
-- return one batch, on every tick of the duty while any edge waited. Over
-- (task_id, other_id) the walk is in the selection's own order and its LIMIT
-- stops it after a batch.
--
-- Still PARTIAL on the two flags, because the selection states both and a
-- healthy company holds no row in it at all, which is what keeps the duty's
-- gate (`pendingOneSided` in internal/worklet/duty.go) a probe that touches
-- nothing.
DROP INDEX tracker_relations_repair_idx;
CREATE INDEX tracker_relations_repair_idx ON tracker_relations (task_id, other_id)
    WHERE one_sided = 1 AND one_sided_final = 0;                                                              -- the relations repair duty's selection, in its order
