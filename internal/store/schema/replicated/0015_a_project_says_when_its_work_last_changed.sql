-- A project row says WHEN its work last changed and WHO changed it.
--
-- # What was missing
--
-- `tracker_projects` carries a maintained census — `open_count`, `done_count`,
-- `closed_count` — and nothing else about the work filed into it. So a
-- directory of projects can render three numbers and cannot answer the
-- question somebody scanning that list is actually asking: which of these is
-- anybody working on. "PLATFORM holds 41 open items" says nothing about
-- whether the last of them moved this morning or last quarter, and the only
-- way to find out was to open each project's own activity feed, one at a time.
--
-- # Why maintained columns rather than a query
--
-- The argument the counts beside them were added on, and it is stronger here.
-- The value is the newest of the project's history rows, and a directory
-- drawing thirty projects would run thirty of those seeks on every poll —
-- against `tracker_history`, which is one row per applied commit and which
-- NOTHING EVER SWEEPS, so the cost grows for the life of the deployment.
-- Maintained, it is four columns the commit that moves them already holds in
-- hand: the applier writes them inside the transaction that writes the history
-- row itself.
--
-- # Why four columns for two facts
--
-- `last_change_at`, `last_change_actor` and `last_change_actor_kind` are what
-- a reader renders — the instant, the handle, and which of the four author
-- kinds that handle belongs to, because `ana` the person and `ana` the seat
-- are different answers and a directory draws them differently.
--
-- `last_change_seq` is the fourth and it is the GUARD. The three above are one
-- record's values copied together, so the row must always name the commit that
-- is highest in the log rather than whichever record this node happened to
-- apply last: two nodes at one checkpoint have seen the same SET of records
-- and a different ORDER of them, and a column folded over arrival order
-- diverges permanently — on a table whose rows a fleet compares byte for byte.
-- The composed log position is a total order every node already agrees on, so
-- `WHERE last_change_seq < ?` makes the write idempotent, order-independent and
-- safe against a redelivery, without a clock comparison anywhere. It is also
-- why an instant alone could not be the guard: two records can share one, and
-- the tie would then be broken by whoever arrived first.
--
-- The instant is the AUTHORED one — the writer's own clock, which is what
-- `tracker_tasks.updated_at` and `tracker_projects.updated_at` beside it
-- already carry and what the activity feed DISPLAYS for the same commit. A
-- surface rendering "last changed" next to those must not be quoting a
-- different kind of instant from the ones above and below it. The effective
-- instant is for durations and report windows; nothing measures a duration on
-- this column.
--
-- # The backfill is exact, and an absent value stays absent
--
-- The maintained rule is "the newest history row about a task in this project",
-- and `tracker_history` holds every one of those rows from the day the domain
-- landed: the applier files a task commit under its project key and a
-- whole-document commit — a project rename, a catalogue edit, a saved view —
-- under no project at all, so a non-empty `project_key` already IS the
-- task-commit set. That makes this backfill the same function of the same
-- table the applier will maintain forward, rather than an approximation of it.
-- The predicate below says `subject_kind = 'task'` anyway, which costs nothing
-- on the same index seek and states the rule instead of relying on it.
--
-- A project with no task commit is left NULL rather than stamped with its own
-- creation, because "nothing has ever been filed here" and "somebody made this
-- project" are different facts and a directory that renders the second as the
-- first reports every empty project as freshly active. NULL is what the reader
-- reports as an absent value.
--
-- One indexed seek per project on `tracker_history_project_idx`, which is
-- `(project_key, log_seq DESC)` — and a company's projects are tens.
--
-- No index of its own: these are read as part of the project's own row, which
-- the primary key already reaches, and the listing orders by key. An index
-- nobody reads is a write cost on every commit with no reader.
--
-- 0002 IS NOT EDITED. `schema_migrations` keys on the filename, so a file that
-- has already run never runs again: a fresh database and an upgraded one
-- converge here, by the same route.

ALTER TABLE tracker_projects ADD COLUMN last_change_at         INTEGER;
ALTER TABLE tracker_projects ADD COLUMN last_change_actor      TEXT    NOT NULL DEFAULT '';
ALTER TABLE tracker_projects ADD COLUMN last_change_actor_kind TEXT    NOT NULL DEFAULT '';
ALTER TABLE tracker_projects ADD COLUMN last_change_seq        INTEGER NOT NULL DEFAULT 0;

UPDATE tracker_projects SET
    last_change_at = (
        SELECT h.created_at FROM tracker_history h
        WHERE h.project_key = tracker_projects.key AND h.subject_kind = 'task'
        ORDER BY h.log_seq DESC LIMIT 1),
    last_change_actor = (
        SELECT h.actor FROM tracker_history h
        WHERE h.project_key = tracker_projects.key AND h.subject_kind = 'task'
        ORDER BY h.log_seq DESC LIMIT 1),
    last_change_actor_kind = (
        SELECT h.actor_kind FROM tracker_history h
        WHERE h.project_key = tracker_projects.key AND h.subject_kind = 'task'
        ORDER BY h.log_seq DESC LIMIT 1),
    last_change_seq = (
        SELECT h.log_seq FROM tracker_history h
        WHERE h.project_key = tracker_projects.key AND h.subject_kind = 'task'
        ORDER BY h.log_seq DESC LIMIT 1)
WHERE EXISTS (
    SELECT 1 FROM tracker_history h
    WHERE h.project_key = tracker_projects.key AND h.subject_kind = 'task');
