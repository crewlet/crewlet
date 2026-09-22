-- Goals leave the native tracker: the object, its owners, its targets and the
-- references those targets counted work through.
--
-- # What went, and why it went whole
--
-- A goal was a workspace-level object with owners and members, a health value
-- somebody set, an append-only history of progress notes, and TARGETS under it
-- that named tasks, projects or a number moved by hand. Its progress was
-- COMPUTED on every read from the targets' own rows and stored nowhere, which
-- is what makes the removal total rather than partial: with the goal gone
-- there is no figure left to keep, no column to preserve and no second answer
-- to reconcile. The three child tables exist only to be scanned for one goal's
-- id, so keeping any of them would leave a table written on every apply, on
-- every node, to answer no question — which is exactly what 0010, 0013 and
-- 0014 removed.
--
-- `tracker_goal_target_refs` also served the task query's `goal=<id>` filter,
-- and that filter goes with it. It was the ONLY reader of the table outside a
-- goal's own read, and a filter naming an object the company no longer has is
-- a key that matches nothing — refused by the query grammar now, which is the
-- honest answer rather than an empty page.
--
-- # The record kind is RETIRED rather than deleted
--
-- `goal` joins `sprint` in [tracker.RetiredKinds]. An older peer publishing
-- during a rolling upgrade, or a goal record still inside
-- `stream.tracker_retention`, carries a record version this build reads
-- perfectly — it is the KIND that is gone — so without the retirement gate it
-- would reach the applier's dispatch, match no case and wedge the newest node
-- in the fleet at that position. The gate drops it while ADVANCING the
-- checkpoint, exactly as a goal's rows would have been dropped here.
--
-- 0002 IS NOT EDITED. `schema_migrations` keys on the filename, so a file that
-- has already run never runs again: a fresh database and an upgraded one
-- converge here, by the same route.

-- The indexes go with their tables; naming them is belt-and-braces for an
-- engine that would keep an index over a dropped table.
DROP INDEX IF EXISTS tracker_goals_group_idx;
DROP INDEX IF EXISTS tracker_goal_owners_handle_idx;
DROP INDEX IF EXISTS tracker_goal_target_refs_ref_idx;

DROP TABLE IF EXISTS tracker_goal_target_refs;
DROP TABLE IF EXISTS tracker_goal_targets;
DROP TABLE IF EXISTS tracker_goal_owners;
DROP TABLE IF EXISTS tracker_goals;
