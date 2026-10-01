-- A notice names the TASK it is about, beside the subject it was written on.
--
-- # What was missing
--
-- A notice stores `subject_id` (the object its record changed) and
-- `subject_key` (the key a reader follows). For a task commit the two describe
-- one task. For the one routable wake that is not a task commit — a lead
-- putting a task at the top of somebody's priorities — the subject is the
-- PERSON and the key is the TASK's, so the row held the task's key and no
-- task id at all: the wake's snapshot named it, and the inbox row dropped it.
--
-- That matters the moment two tasks hold one key. The key opens the task that
-- claimed it first, so a notice must be able to say "this key does not open
-- the task I mean, and here is the id that does" — and a prioritised notice
-- about the duplicate had no id to say it with. Asked against `subject_id`
-- instead, every prioritised notice compared a task's key with a person's
-- handle, called every one of them a collision, and sent the reader to
-- `#/work/<handle>`.
--
-- # The column
--
-- `task_id` is the wake's own `Snapshot.TaskID`: the subject for a task
-- commit, the task the snapshot names for a person's priorities, and empty for
-- a wake that names no task. NOT NULL with a default, because "names no task"
-- is a state rather than a missing value.
--
-- # The backfill
--
-- A task commit's notice is filled from its own subject, which is exactly
-- what the applier now writes for one. It is recognised by its history row's
-- subject kind, OR by a subject that is a task row: a notice outlives a
-- history row a reanchor took, and a person's handle is never a task's id.
-- A prioritised notice written before
-- this column is left empty: the task its wake named is in no row this table
-- can reach, and guessing it from the key would answer with the claimant —
-- the one task such a notice may NOT mean. An empty id reads as "the key is
-- the only address", which is what every such notice was before.
--
-- No index: the column is read only beside the row the inbox range already
-- reaches.
--
-- 0002 IS NOT EDITED. `schema_migrations` keys on the filename, so a file
-- that has already run never runs again: a fresh database and an upgraded one
-- converge here, by the same route.

ALTER TABLE tracker_notifications ADD COLUMN task_id TEXT NOT NULL DEFAULT '';

UPDATE tracker_notifications SET task_id = subject_id
 WHERE record_id IN (SELECT id FROM tracker_history WHERE subject_kind = 'task')
    OR subject_id IN (SELECT id FROM tracker_tasks);
