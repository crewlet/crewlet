-- The trash is read through an index that holds it, at both scopes.
--
-- # tracker_tasks_removed_idx, tracker_tasks_project_removed_idx
--
-- 0002 declared `tracker_tasks_removed_idx (removed_at DESC) WHERE removed_at
-- IS NOT NULL` for the trash, and no statement of the trash reached it. A
-- listing is ordered `removed_at DESC, id` — the id is what makes a page
-- boundary stable — and an index that stops at the instant cannot give the
-- tiebreak, so the planner read every task in the company from the heap and
-- sorted them, and inside a project it sought the project's whole board
-- through `tracker_tasks_rank_idx` and sorted that. Every statement over the
-- trash that is not ordered at all — the count an answer carries, its totals,
-- a board's columns, the subtree rollup's subquery, a pinned view's count —
-- read the whole table at either scope, because this engine's planner reaches
-- a partial index only for an order it provides or a seek on its leading
-- column, and `removed_at IS NOT NULL` is neither.
--
-- So the workspace index carries the whole order, and a project's trash has an
-- index of its own that leads with the project, as the board and the recent
-- listing do (`tracker_tasks_board_idx`, `tracker_tasks_recent_idx`): a
-- project's trash is then a seek on its key in the trash's own order rather
-- than a walk of the company's trash filtered by project. Both stay partial on
-- the tombstone, so each is the size of the trash rather than of the table and
-- a live task's commit writes neither — and every statement of the trash
-- NAMES the one its scope reads (`taskSource` in internal/worklet/read.go),
-- which is what makes the unordered ones reach it at all.
--
-- 0002 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change.
DROP INDEX IF EXISTS tracker_tasks_removed_idx;
CREATE INDEX tracker_tasks_removed_idx ON tracker_tasks (removed_at DESC, id)
    WHERE removed_at IS NOT NULL;                                                                             -- the trash: removed=true at workspace scope, and every statement over it
CREATE INDEX tracker_tasks_project_removed_idx ON tracker_tasks (project_key, removed_at DESC, id)
    WHERE removed_at IS NOT NULL;                                                                             -- the trash inside a project
