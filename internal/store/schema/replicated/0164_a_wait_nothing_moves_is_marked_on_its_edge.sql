-- A wait on a pull request that nothing moves is marked on its dependency edge,
-- so the ready work reads an item whose every open wait is such a wait as ready
-- for its holder (ADR-0128).
--
-- WHO HAS TO AGREE ON IT: every node applying the same record writes the same
-- value. `dead_head` is derived on every task apply from the task's own record
-- (Task.DeadWaits): the head commit the engine read the change at when nothing
-- moved it, and '' for every other edge, every edge on a task, and every row an
-- older build wrote.
ALTER TABLE tracker_task_deps ADD COLUMN dead_head TEXT NOT NULL DEFAULT '';
