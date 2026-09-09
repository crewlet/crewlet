-- The tracker's projection leaves this estate, because the tracker is no
-- longer projected.
--
-- 0020 created these seven tables as a REBUILDABLE local copy of the work
-- family's bucket: a projector followed the change feed and wrote them, and a
-- board read them. `0002_the_tracker_lands.sql` in the REPLICATED estate is
-- what replaced them — the same rows, written by a deterministic applier from
-- an ordered stream, with the checkpoint committed in the same transaction.
--
-- The move is not a relocation of a cache. A projection is per-node state
-- with a cursor that can be behind and no way to say how far; the applier's
-- copy has a position on the log, so a read reports the level it answered at
-- and a caller can wait for its own write. That is the whole reason the
-- tracker moved estates, and it is why nothing here is copied across: a
-- statement may not name a table in both files, and every row these tables
-- hold is derivable from the log by replaying it — which is what a node with
-- no tracker rows does on its first boot after this migration.
--
-- CHILDREN FIRST. Every one of these carries `REFERENCES work_items(id) ON
-- DELETE CASCADE`, and dropping the parent while a child still references it
-- leaves the child's foreign key pointing at a table that is gone: the next
-- write to it fails on a constraint naming a table nobody can find, and the
-- error names neither this migration nor the table that left.
DROP TABLE work_history;
DROP TABLE work_comments;
DROP TABLE work_links;
DROP TABLE work_watchers;
DROP TABLE work_labels;

-- The parent, and then the one table that was never a child: the counter
-- cache. It is not carried across either — a mint reads the project's own
-- counter subject and lets the broker arbitrate, so there is nothing local
-- for it to be a stale copy of.
DROP TABLE work_items;
DROP TABLE work_counters;
