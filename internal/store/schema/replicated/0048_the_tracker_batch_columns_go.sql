-- The tracker's batch columns, and the two indexes over them, go.
--
-- # tracker_tasks.batch_id, tracker_history.batch_id
--
-- 0002 declared a batch id on a task's row and on every history row for a
-- bulk edit that would stamp each commit it made with one id, so a `batch=`
-- filter could list what one call touched and the notifications it wrote could
-- be told apart as facts rather than asks. No writer ever set one: the record
-- field the appliers copied into both columns was assigned by nothing, and the
-- bulk edit itself, the only verb that could have, is gone. Both columns held
-- NULL on every row of every node, and `batch=` on the work list and the
-- activity feed answered nothing at all.
--
-- # tracker_tasks_batch_idx, tracker_history_batch_idx
--
-- Each claimed the `batch=` filter as its reader. That filter is gone with the
-- columns, and an index no plan reaches is a write cost on every commit on
-- every node for ever.
--
-- The indexes go first: Turso drops a column no index, view or trigger names.
-- Dropping is safe across a rolling upgrade because no build ever wrote either
-- column anything but NULL, and the only statements naming them are the
-- appliers' own, which are this binary's.
--
-- 0002 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change.
DROP INDEX IF EXISTS tracker_tasks_batch_idx;
DROP INDEX IF EXISTS tracker_history_batch_idx;
ALTER TABLE tracker_tasks DROP COLUMN batch_id;
ALTER TABLE tracker_history DROP COLUMN batch_id;
