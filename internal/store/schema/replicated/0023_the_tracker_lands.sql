-- The native tracker's durable state: the read side of the mutation log.
--
-- Every table here is REBUILT FROM THE LOG rather than written by a caller.
-- The applier is the only writer, its transaction carries the checkpoint, and
-- an identity claim compares these rows byte for byte across the fleet — so
-- anything a node decides for itself belongs in the node's own estate instead.
--
-- # Three rules the schema enforces, and one it deliberately does not
--
-- NO FOREIGN KEY ANYWHERE. A cascade is a delete nobody committed: the applier
-- removes a parent's children in the same statement list, where the deletion is
-- part of the record's own effect and therefore identical on every node.
--
-- NO `UNIQUE` OUTSIDE A PRIMARY KEY. A constraint violation inside the apply
-- transaction aborts it deterministically on every node — which turns a rare
-- cosmetic anomaly into a fleet-wide stalled log. `tracker_tasks.key` is the
-- worked example: a restored counter beside newer tasks flags `key_collision`
-- and keeps serving, where a unique index would wedge every node at once.
--
-- NO `COLLATE` ANYWHERE. The default TEXT collation is BINARY, so `ORDER BY
-- rank` reproduces the Go comparison exactly. The pinned driver ACCEPTS
-- `COLLATE NOCASE` (measured), which would silently collapse 'A' and 'a' and
-- destroy the manual order, so a test reads this DDL back out of the schema
-- table rather than trusting the absence.
--
-- What it does NOT enforce is the object versions' monotonicity: that is the
-- applier's `WHERE excluded.version > version` guard, because it has to be a
-- SKIP rather than an error — a redelivered record is ordinary traffic.
--
-- # Every index names the query it serves
--
-- In a trailing comment, and a test runs EXPLAIN QUERY PLAN over the
-- registered query set and asserts every index appears in at least one plan.
-- Twenty-six indexes on the hottest table is a write cost the apply drain has
-- to carry on every commit, and an index nobody reads is that cost with no
-- reader — which is how two partial indexes with no writer at all survived a
-- design review.

-- ---------------------------------------------------------------------------
-- The object tables.
--
-- Nine of them carry `version INTEGER NOT NULL` — the COMPOSED
-- (generation << 40) | stream sequence of the commit that last arbitrated this
-- object, in the same integer space as the domain's own checkpoint — and seven
-- of those nine also carry `document`, the object's full encoded state
-- INCLUDING fields this build does not know, so a rolling upgrade's newer
-- fields round-trip through a durable row.
--
-- `tracker_counters` and `tracker_persons` carry `version` without `document`,
-- because every field of each is already a column and a document would be a
-- second copy of the row. The exploded tables carry neither: a comment, a body
-- revision, an alias, a tag, a type, a field declaration and a field option are
-- arbitrated by the object they belong to.
--
-- NO OBJECT TABLE CARRIES A GENERATION COLUMN. The generation is the high bits
-- of `version`, which is what keeps every upsert guard a bare integer
-- comparison and makes a reanchor a value change rather than a schema change.
-- ---------------------------------------------------------------------------

CREATE TABLE tracker_tasks (
    id                   TEXT    NOT NULL PRIMARY KEY,
    -- NOT UNIQUE, deliberately: see the header.
    key                  TEXT    NOT NULL,
    project_key          TEXT    NOT NULL,
    -- filed_unit is IMMUTABLE and is a record of what was true; routing_unit
    -- is the mutable half, and the only unit column any write touches.
    filed_unit           TEXT    NOT NULL DEFAULT '',
    routing_unit         TEXT    NOT NULL DEFAULT '',
    sprint_number        INTEGER,
    parent_id            TEXT,
    root_id              TEXT    NOT NULL,
    depth                INTEGER NOT NULL DEFAULT 0,
    type                 TEXT    NOT NULL,
    title                TEXT    NOT NULL,
    -- `body` is NOT a column: `document` already carries the current body, the
    -- reader decodes it for every single-task read anyway, nothing indexes it,
    -- and the duplicate costs ≈ 200 MB a year.
    body_version         INTEGER NOT NULL DEFAULT 0,
    status               TEXT    NOT NULL,
    status_group         TEXT    NOT NULL,
    -- The EFFECTIVE instant, because it is the start of a duration. It is
    -- rewritten by the wholesale span recompute, which hangs off the HISTORY
    -- row's write rather than this one: the object-row update is dropped by the
    -- version guard on a reprocessing node, and a column nothing repairs
    -- diverges permanently.
    status_entered_at    INTEGER NOT NULL DEFAULT 0,
    priority             TEXT    NOT NULL DEFAULT 'none',
    prio_rank            INTEGER NOT NULL DEFAULT 0,
    -- The alphabet is a CHECK because the ordering is the whole feature. The
    -- obvious spelling GLOB '[0-9A-Za-z]*' ACCEPTS 'a-0' (measured: it means
    -- "one character from the class, then anything"), so the negated form is
    -- what actually constrains it. 254 is an ASSERTION rather than a policy:
    -- any mint that would exceed 64 is replaced by a re-spread, so a key this
    -- long can only mean the generator is broken.
    rank                 TEXT    NOT NULL
                             CHECK (length(rank) BETWEEN 2 AND 254
                                    AND rank NOT GLOB '*[^0-9A-Za-z]*'),
    reporter             TEXT    NOT NULL DEFAULT '',
    assignee             TEXT    NOT NULL DEFAULT '',
    start_at             INTEGER,
    due_at               INTEGER,
    due_all_day          INTEGER NOT NULL DEFAULT 0,
    estimate_min         INTEGER NOT NULL DEFAULT 0,
    points               REAL    NOT NULL DEFAULT 0,
    -- SEVEN counters and one derived sort key, from one list in Go: the struct,
    -- this DDL and the applier's statement are generated from it, because three
    -- hand-written copies of eight column names is three chances to add the
    -- ninth to two of them.
    spend_turns          INTEGER NOT NULL DEFAULT 0,
    spend_rounds         INTEGER NOT NULL DEFAULT 0,
    spend_input          INTEGER NOT NULL DEFAULT 0,
    spend_output         INTEGER NOT NULL DEFAULT 0,
    spend_cache_read     INTEGER NOT NULL DEFAULT 0,
    spend_cache_write    INTEGER NOT NULL DEFAULT 0,
    spend_wall_ms        INTEGER NOT NULL DEFAULT 0,
    spend_tokens         INTEGER NOT NULL DEFAULT 0,
    -- Stamped by GROUP rather than by slug: done OR cancelled sets done_at.
    done_at              INTEGER,
    closed_at            INTEGER,
    finished_at          INTEGER,
    archived             INTEGER NOT NULL DEFAULT 0,
    archived_at          INTEGER,
    removed_at           INTEGER,
    removed_with         TEXT,
    batch_id             TEXT,
    -- Set while a merge's child walk is running and cleared by its last
    -- append, so a duplicate is visibly MID-MERGE rather than silently
    -- half-merged. It is what the duty selects on to finish an abandoned
    -- walk, and the difference between a state somebody can wait out and one
    -- they have to reconstruct.
    merging              INTEGER NOT NULL DEFAULT 0,
    reassignments        INTEGER NOT NULL DEFAULT 0,
    policy_stamp         TEXT    NOT NULL DEFAULT '',
    -- A MAX over every applied commit that named this task as unblocked, so it
    -- is order-independent and identical on every node.
    unblocked_told_at    INTEGER,
    search_rev           INTEGER NOT NULL DEFAULT 0,
    embed_rev            INTEGER NOT NULL DEFAULT 0,
    -- The four attention flags. An applier that cannot satisfy a structural
    -- rule APPLIES THE RECORD COMPLETELY and raises a flag, because refusing
    -- would stall the log on a shape two concurrent writers can legally create.
    inconsistent_project INTEGER NOT NULL DEFAULT 0,
    cycle                INTEGER NOT NULL DEFAULT 0,
    too_deep             INTEGER NOT NULL DEFAULT 0,
    key_collision        INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    version              INTEGER NOT NULL,
    -- The highest record that wrote this row FROM ANOTHER SUBJECT. A rank move
    -- stamps this and never `version`, so the row's own broker expectation
    -- still matches its subject's last message and cannot be poisoned into
    -- permanent unwritability; a read barrier compares MAX of the two.
    scoped_through       INTEGER NOT NULL DEFAULT 0,
    document             BLOB    NOT NULL
);

-- NINE INDEXES THE PLANNER NEVER CHOSE ARE NOT HERE, and their absence is a
-- measurement rather than an oversight.
--
-- Five were shadowed by the board index's own seek: `(project_key,
-- status_group, status, rank)`, `(project_key, sprint_number, …)`,
-- `(parent_id, …)`, `(root_id, …)` and `(project_key, archived)`. Inside a
-- project that seek already narrows to a thirtieth of the corpus and the
-- residual predicate rides along on those rows; outside one, a sprint number
-- and a parent id are not questions the grammar asks.
--
-- Four more — `(status, …)`, `(due_at, …)`, `(start_at, …)`, `(finished_at,
-- …)` and the four-flag attention index — lost to the ORDER BY. Every list
-- query here is ordered and limited, and under a limit this engine's planner
-- prefers walking the ordering index to seeking a filter and sorting: measured
-- at twenty thousand rows over thirty projects with a tenth of the corpus
-- carrying a due date, the plan is the ordering scan whether or not the filter
-- index exists. What WOULD reach them is an aggregate with no order — a total,
-- a burndown — and the step that adds those readers adds the index with the
-- plan that proves it.
--
-- Each was a write cost on every one of a year's ≈ 2.4 M commits, on every
-- node, for a plan that never ran.
--
-- EVERY ORDERED INDEX ENDS IN `id`, because every ordered read does.
--
-- [orderBy] appends `t.id` to every sort as the tiebreak that makes a page
-- stable, so an index that stops one column short does not match the order the
-- query asks for — and this engine's planner does not then seek it and sort a
-- hundred rows, it ABANDONS the index and scans the table. Measured: the same
-- board query orders by `rank, id` and seeks, and by `updated_at DESC, id` and
-- scans, against indexes differing only in that last column.
--
-- EVERY QUERY-SERVING INDEX CARRIES `WHERE removed_at IS NULL`, and it is not
-- a size optimisation — it is what makes the planner reach for them at all.
--
-- Every registered query filters removed tasks out, and SQLite's planner
-- treats a partial index as usable only when the query implies its `WHERE`.
-- The embed duty's index was the only one carrying that predicate, so for a
-- board read — `project_key = ? AND removed_at IS NULL ORDER BY rank` —
-- the planner picked `(embed_rev) WHERE removed_at IS NULL`, SCANNED it and
-- SORTED, in preference to seeking a full index that gave it both the project
-- and the order. Measured on 400 rows and on the same query with the predicate
-- removed, which seeks: the cost of the plan is a property of the predicate,
-- not of the fixture. `TestEveryIndexServesARegisteredQuery` is what says so.
--
-- The exceptions are the three that must see a removed task — key resolution,
-- the trash, and the subtree restore — plus the apply-path probes, which are
-- about a row rather than about a query.

-- Fifteen plain indexes, each naming its reader.
CREATE INDEX tracker_tasks_key_idx ON tracker_tasks (key);                                                    -- key resolution, and every former-key lookup that lands here (a reference to a REMOVED task still resolves, which is why this one is not partial)
CREATE INDEX tracker_tasks_rank_idx ON tracker_tasks (project_key, rank, id);                                 -- the apply's per-key duplicate probe, which is about a row and carries no query's predicates
CREATE INDEX tracker_tasks_board_idx ON tracker_tasks (project_key, rank, id)
    WHERE removed_at IS NULL;                                                                                 -- the board: list_tasks(container=project:, sort=rank), and every project-scoped read that orders
CREATE INDEX tracker_tasks_recent_idx ON tracker_tasks (project_key, updated_at DESC, id)
    WHERE removed_at IS NULL;                                                                                 -- list_tasks(sort=-updated) inside a project
CREATE INDEX tracker_tasks_queue_idx ON tracker_tasks (assignee, status_group, prio_rank DESC, due_at)
    WHERE removed_at IS NULL;                                                                                 -- preset=my_queue, and every assignee= filter
CREATE INDEX tracker_tasks_spend_idx ON tracker_tasks (spend_tokens DESC, id) WHERE removed_at IS NULL;       -- sort=spend, spend= filters, totals spend_tokens:sum
CREATE INDEX tracker_tasks_estimate_idx ON tracker_tasks (estimate_min, id) WHERE removed_at IS NULL;         -- estimate= filters and totals estimate_min:sum
CREATE INDEX tracker_tasks_points_idx ON tracker_tasks (points, id) WHERE removed_at IS NULL;                 -- points= filters, totals points:sum, sprint capacity
CREATE INDEX tracker_tasks_batch_idx ON tracker_tasks (batch_id) WHERE removed_at IS NULL;                    -- batch= (what one bulk call touched)
CREATE INDEX tracker_tasks_filed_unit_idx ON tracker_tasks (filed_unit) WHERE removed_at IS NULL;             -- unit= over every spelling a unit has been keyed by
CREATE INDEX tracker_tasks_routing_unit_idx ON tracker_tasks (routing_unit) WHERE removed_at IS NULL;         -- routing_unit=, the lead's own queue
CREATE INDEX tracker_tasks_type_idx ON tracker_tasks (type) WHERE removed_at IS NULL;                         -- type= and group_by=type
CREATE INDEX tracker_tasks_updated_idx ON tracker_tasks (updated_at DESC, id) WHERE removed_at IS NULL;       -- sort=-updated at workspace scope, the default outside a project

-- The rest are partial on something OTHER than the tombstone. Each `WHERE` is
-- what keeps the index the size of the answer rather than the size of the
-- table.
CREATE INDEX tracker_tasks_removed_idx ON tracker_tasks (removed_at DESC) WHERE removed_at IS NOT NULL;       -- work_trash
CREATE INDEX tracker_tasks_removed_with_idx ON tracker_tasks (removed_with) WHERE removed_with IS NOT NULL;   -- restoring a subtree removed together
CREATE INDEX tracker_tasks_embed_idx ON tracker_tasks (embed_rev) WHERE removed_at IS NULL;                   -- the embed duty's selection
CREATE INDEX tracker_tasks_respread_idx ON tracker_tasks (project_key, rank, id) WHERE length(rank) > 64;     -- the rank duty's order-preserving re-spread walk (a fraction of the board index, which is why both)
CREATE INDEX tracker_tasks_merging_idx ON tracker_tasks (id) WHERE merging = 1;                               -- the tracker duty's selection of an abandoned merge walk

CREATE TABLE tracker_comments (
    id           TEXT    NOT NULL PRIMARY KEY,
    task_id      TEXT    NOT NULL,
    author       TEXT    NOT NULL,
    author_kind  TEXT    NOT NULL,
    body         TEXT    NOT NULL,
    reply_to     TEXT,
    ask          TEXT    NOT NULL DEFAULT '',
    answers      TEXT,
    answered_by  TEXT,
    resolved     INTEGER NOT NULL DEFAULT 0,
    resolved_by  TEXT,
    resolved_at  INTEGER,
    -- A removal blanks the body and KEEPS the row, so replies still resolve.
    removed      INTEGER NOT NULL DEFAULT 0,
    record_id    TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    document     BLOB    NOT NULL
    -- NO `version`: a comment is arbitrated by its task.
);
CREATE INDEX tracker_comments_thread_idx ON tracker_comments (task_id, reply_to, created_at DESC);            -- get_task(include=comments) and one thread
CREATE INDEX tracker_comments_answers_idx ON tracker_comments (answers);                                      -- resolving an ask through its answer
CREATE INDEX tracker_comments_open_asks_idx ON tracker_comments (task_id, ask)
    WHERE ask <> '' AND resolved = 0 AND answered_by IS NULL AND removed = 0;                                 -- has_open_asks, and get_task's open_asks[]
CREATE INDEX tracker_comments_asked_of_idx ON tracker_comments (author)
    WHERE ask <> '' AND resolved = 0 AND answered_by IS NULL;                                                 -- asked_by= (who is waiting on an answer)

CREATE TABLE tracker_body_revisions (
    task_id     TEXT    NOT NULL,
    -- The BODY version, not a log position. Deliberately not renamed: the
    -- primary key is what a reader meets first and it reads correctly there.
    version     INTEGER NOT NULL,
    author      TEXT    NOT NULL DEFAULT '',
    author_kind TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL,
    size        INTEGER NOT NULL DEFAULT 0,
    document    BLOB    NOT NULL,
    PRIMARY KEY (task_id, version)
);
CREATE INDEX tracker_body_revisions_recent_idx ON tracker_body_revisions (task_id, version DESC);             -- get_task(include=revisions), and the commit's own prune

-- The key directory. UPSERTED, never deleted by a task apply, and never
-- lowered from current: a former key must go on resolving for the life of the
-- deployment, because it is pasted into chat and typed into tool calls.
CREATE TABLE tracker_task_keys (
    key     TEXT    NOT NULL PRIMARY KEY,
    task_id TEXT    NOT NULL,
    current INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX tracker_task_keys_task_idx ON tracker_task_keys (task_id);                                       -- a task's former keys, and the alias claim's guard row

CREATE TABLE tracker_projects (
    key                    TEXT    NOT NULL PRIMARY KEY,
    -- Chart-owned: written only by the chart apply, under the epoch guard.
    name                   TEXT    NOT NULL,
    purpose                TEXT    NOT NULL DEFAULT '',
    unit                   TEXT    NOT NULL DEFAULT '',
    chart_epoch            INTEGER NOT NULL DEFAULT 0,
    default_assignee       TEXT    NOT NULL DEFAULT '',
    sprint_policy_json     TEXT,
    sprint_next            INTEGER NOT NULL DEFAULT 1,
    active_sprint          INTEGER,
    sprint_measure         TEXT    NOT NULL DEFAULT 'points',
    policy_version         INTEGER NOT NULL DEFAULT 0,
    archived               INTEGER NOT NULL DEFAULT 0,
    -- Set by a drag whose inline re-spread window would exceed its cap and
    -- cleared by the walk's last record.
    rank_respread_pending  INTEGER NOT NULL DEFAULT 0,
    -- Set by the APPLIER from one indexed probe when it writes a rank a sibling
    -- already holds, and cleared the same way — which is what makes a duplicate
    -- a repairable observable rather than only a published number.
    rank_duplicate_pending INTEGER NOT NULL DEFAULT 0,
    -- MAINTAINED, never scanned: updated in the task apply when a status group
    -- changes or a task enters, leaves or is removed from the project. One row
    -- per commit, a pure function of the applied records — against an aggregate
    -- over every task in every project on every sixty-second poll.
    open_count             INTEGER NOT NULL DEFAULT 0,
    done_count             INTEGER NOT NULL DEFAULT 0,
    closed_count           INTEGER NOT NULL DEFAULT 0,
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL,
    version                INTEGER NOT NULL,
    document               BLOB    NOT NULL
);
CREATE INDEX tracker_projects_unit_idx ON tracker_projects (unit);                                            -- work_projects grouped by unit, and the chart apply
CREATE INDEX tracker_projects_archived_idx ON tracker_projects (archived);                                    -- the default archived=false join from tracker_tasks

CREATE TABLE tracker_sprints (
    project_key   TEXT    NOT NULL,
    number        INTEGER NOT NULL,
    name          TEXT    NOT NULL,
    goal          TEXT    NOT NULL DEFAULT '',
    start_at      INTEGER NOT NULL,
    end_at        INTEGER NOT NULL,
    state         TEXT    NOT NULL,
    closed_at     INTEGER,
    closed_by     TEXT,
    open_at_close INTEGER NOT NULL DEFAULT 0,
    -- EMPTY while the spillover is pending, which makes pending a state rather
    -- than an absence somebody has to interpret.
    rollover_to    TEXT,
    rollover_done  INTEGER NOT NULL DEFAULT 0,
    archived       INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    version        INTEGER NOT NULL,
    document       BLOB    NOT NULL,
    PRIMARY KEY (project_key, number)
);
CREATE INDEX tracker_sprints_state_idx ON tracker_sprints (project_key, state, number);                       -- sprint=future|closed, and the sprint list
CREATE INDEX tracker_sprints_spillover_idx ON tracker_sprints (project_key, number)
    WHERE state = 'closed' AND rollover_done = 0 AND rollover_to IS NOT NULL;                                 -- the rollover walk's own selection
CREATE INDEX tracker_sprints_unsettled_idx ON tracker_sprints (project_key, number)
    WHERE state = 'closed' AND rollover_to IS NULL;                                                           -- a close whose spillover nobody has decided
CREATE INDEX tracker_sprints_active_idx ON tracker_sprints (project_key) WHERE state = 'active';              -- sprint=active without reading the project's pointer

-- `version` without `document`: every field is already a column, and a document
-- would be a second copy of the row.
CREATE TABLE tracker_counters (
    project_key TEXT    NOT NULL PRIMARY KEY,
    last        INTEGER NOT NULL DEFAULT 0,
    version     INTEGER NOT NULL
);

CREATE TABLE tracker_tagsets (
    project_key  TEXT    NOT NULL PRIMARY KEY,
    tags_version INTEGER NOT NULL DEFAULT 0,
    version      INTEGER NOT NULL,
    document     BLOB    NOT NULL
);

CREATE TABLE tracker_catalogues (
    name     TEXT NOT NULL PRIMARY KEY,
    version  INTEGER NOT NULL,
    document BLOB    NOT NULL
);

-- Exploded from the tag set. `label_norm` is the case-folded label, stored
-- rather than computed, because the collision rule is case-insensitive and a
-- `lower()` in the predicate cannot use an index.
CREATE TABLE tracker_tags (
    project_key  TEXT    NOT NULL,
    slug         TEXT    NOT NULL,
    label        TEXT    NOT NULL,
    label_norm   TEXT    NOT NULL,
    color        TEXT    NOT NULL DEFAULT '',
    description  TEXT    NOT NULL DEFAULT '',
    archived     INTEGER NOT NULL DEFAULT 0,
    tags_version INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_key, slug)
);
CREATE INDEX tracker_tags_label_idx ON tracker_tags (project_key, label_norm);                                -- tag resolution by label, and the collision refusal

CREATE TABLE tracker_types (
    slug        TEXT    NOT NULL PRIMARY KEY,
    name        TEXT    NOT NULL,
    name_norm   TEXT    NOT NULL,
    plural      TEXT    NOT NULL DEFAULT '',
    icon        TEXT    NOT NULL DEFAULT '',
    description TEXT    NOT NULL DEFAULT '',
    builtin     INTEGER NOT NULL DEFAULT 0,
    archived    INTEGER NOT NULL DEFAULT 0
);

-- Exploded from the two declaring documents. The primary key carries the SCOPE
-- because one field id may legitimately sit in two documents for the length of
-- a move between them; `shadowed` is what a reader sees meanwhile.
CREATE TABLE tracker_fields (
    id                   TEXT    NOT NULL,
    scope_kind           TEXT    NOT NULL,
    scope_id             TEXT    NOT NULL,
    slug                 TEXT    NOT NULL,
    name                 TEXT    NOT NULL,
    name_norm            TEXT    NOT NULL,
    description          TEXT    NOT NULL DEFAULT '',
    type                 TEXT    NOT NULL,
    applies_to_json      TEXT    NOT NULL DEFAULT '[]',
    required             INTEGER NOT NULL DEFAULT 0,
    required_in_subtasks INTEGER NOT NULL DEFAULT 0,
    archived             INTEGER NOT NULL DEFAULT 0,
    pinned               INTEGER NOT NULL DEFAULT 0,
    hide_from_agents     INTEGER NOT NULL DEFAULT 0,
    config_json          TEXT    NOT NULL DEFAULT '{}',
    default_json         TEXT,
    shadowed             INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (id, scope_kind, scope_id)
);
CREATE INDEX tracker_fields_scope_idx ON tracker_fields (scope_kind, scope_id, slug);                         -- describe_project's effective union, and f.<slug> resolution
CREATE INDEX tracker_fields_slug_idx ON tracker_fields (slug);                                                -- f.<slug> across both levels, and the collision refusal
CREATE INDEX tracker_fields_name_idx ON tracker_fields (name_norm);                                           -- resolution by label, and the case-insensitive collision rule
CREATE INDEX tracker_fields_id_idx ON tracker_fields (id);                                                    -- f.<id>, and the shadowed pair during a move

CREATE TABLE tracker_field_options (
    field_id  TEXT    NOT NULL,
    option_id TEXT    NOT NULL,
    slug      TEXT    NOT NULL,
    name      TEXT    NOT NULL,
    name_norm TEXT    NOT NULL,
    color     TEXT    NOT NULL DEFAULT '',
    ord       INTEGER NOT NULL DEFAULT 0,
    archived  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (field_id, option_id)
);
CREATE INDEX tracker_field_options_slug_idx ON tracker_field_options (field_id, slug);                        -- option resolution by slug on a dropdown filter
CREATE INDEX tracker_field_options_name_idx ON tracker_field_options (field_id, name_norm);                   -- option resolution by label

CREATE TABLE tracker_views (
    id             TEXT    NOT NULL PRIMARY KEY,
    container_kind TEXT    NOT NULL,
    container_id   TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    owner          TEXT    NOT NULL DEFAULT '',
    protected      INTEGER NOT NULL DEFAULT 0,
    is_default     INTEGER NOT NULL DEFAULT 0,
    rank           TEXT    NOT NULL DEFAULT '',
    icon           TEXT    NOT NULL DEFAULT '',
    params_json    TEXT    NOT NULL DEFAULT '{}',
    version        INTEGER NOT NULL,
    document       BLOB    NOT NULL
);
CREATE INDEX tracker_views_container_idx ON tracker_views (container_kind, container_id, owner, rank);         -- the view list for a container, personal views included

CREATE TABLE tracker_goals (
    id          TEXT    NOT NULL PRIMARY KEY,
    name        TEXT    NOT NULL,
    owners_json TEXT    NOT NULL DEFAULT '[]',
    group_label TEXT    NOT NULL DEFAULT '',
    start_at    INTEGER,
    due_at      INTEGER,
    health      TEXT    NOT NULL DEFAULT '',
    archived    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    version     INTEGER NOT NULL,
    document    BLOB    NOT NULL
);
CREATE INDEX tracker_goals_group_idx ON tracker_goals (group_label);                                          -- work_goals grouped by its free label

-- `version` without `document`, for the same reason as the counter. The
-- `generation` here is the INBOX generation and is a different thing from the
-- log generation — named in both places so a reader meets the distinction —
-- while `seen_through` is a composed position and already carries the log
-- generation in its high bits.
CREATE TABLE tracker_persons (
    handle               TEXT    NOT NULL PRIMARY KEY,
    generation           TEXT    NOT NULL DEFAULT '',
    seen_through         INTEGER NOT NULL DEFAULT 0,
    seen_through_stream  TEXT    NOT NULL DEFAULT '',
    read_json            TEXT    NOT NULL DEFAULT '[]',
    unread_json          TEXT    NOT NULL DEFAULT '[]',
    snoozed_json         TEXT    NOT NULL DEFAULT '[]',
    primary_reasons_json TEXT    NOT NULL DEFAULT '[]',
    priorities_json      TEXT    NOT NULL DEFAULT '[]',
    pinned_views_json    TEXT    NOT NULL DEFAULT '[]',
    favorites_json       TEXT    NOT NULL DEFAULT '[]',
    version              INTEGER NOT NULL
);

-- ---------------------------------------------------------------------------
-- The exploded child tables. Each one exists because a FILTER reads it: a
-- collection inside `document` cannot be indexed, and a query that has to
-- decode every row's document is a full scan wearing an index's name.
-- ---------------------------------------------------------------------------

-- The transitive closure, maintained by the applier with a VISITED set rather
-- than a fixed depth — because two concurrent re-parents on two nodes can form
-- a cycle no single write could see.
CREATE TABLE tracker_task_closure (
    ancestor_id   TEXT    NOT NULL,
    descendant_id TEXT    NOT NULL,
    distance      INTEGER NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id)
);
CREATE INDEX tracker_task_closure_up_idx ON tracker_task_closure (descendant_id, distance);                   -- root=, the depth cap, and children_span

-- One row per STAY, which is what every sprint report is derived from:
-- committed, added, removed, remaining and velocity.
CREATE TABLE tracker_task_sprints (
    task_id   TEXT    NOT NULL,
    sprint    INTEGER NOT NULL,
    project_key TEXT  NOT NULL,
    from_at   INTEGER NOT NULL,
    to_at     INTEGER,
    rolled_to INTEGER,
    PRIMARY KEY (task_id, sprint, from_at)
);
CREATE INDEX tracker_task_sprints_sprint_idx ON tracker_task_sprints (project_key, sprint, task_id);          -- sprint=<closed number>, and every sprint report

CREATE TABLE tracker_collaborators (
    task_id TEXT NOT NULL,
    handle  TEXT NOT NULL,
    PRIMARY KEY (task_id, handle)
);
CREATE INDEX tracker_collaborators_handle_idx ON tracker_collaborators (handle, task_id);                     -- collaborator= filter

-- The MUTE travels with the watch, because "not a watcher" and "watching but
-- muted" are different facts: a replay that saw only the difference would
-- silently re-add every unwatched person on the next mention.
CREATE TABLE tracker_watchers (
    task_id TEXT    NOT NULL,
    handle  TEXT    NOT NULL,
    muted   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (task_id, handle)
);
CREATE INDEX tracker_watchers_handle_idx ON tracker_watchers (handle, task_id);                               -- watcher= filter, and the routing snapshot's own read

CREATE TABLE tracker_task_tags (
    task_id     TEXT NOT NULL,
    project_key TEXT NOT NULL,
    slug        TEXT NOT NULL,
    PRIMARY KEY (task_id, slug)
);
CREATE INDEX tracker_task_tags_slug_idx ON tracker_task_tags (project_key, slug, task_id);                    -- tag=any:|all:|none:, and group_by=tag

-- One row per value, exploded BY TYPE so a filter compares a column rather than
-- a JSON extraction. `hidden` and `kind` carry the two non-live states: every
-- filter and total adds `hidden = 0 AND kind <> 'foreign'`.
CREATE TABLE tracker_field_values (
    task_id  TEXT    NOT NULL,
    field_id TEXT    NOT NULL,
    seq      INTEGER NOT NULL DEFAULT 0,
    kind     TEXT    NOT NULL,
    hidden   INTEGER NOT NULL DEFAULT 0,
    num      REAL,
    text     TEXT,
    at       INTEGER,
    ref      TEXT,
    PRIMARY KEY (task_id, field_id, seq)
);
CREATE INDEX tracker_field_values_num_idx ON tracker_field_values (field_id, num) WHERE hidden = 0;           -- f.<slug> numeric, checkbox and progress filters, and their totals
CREATE INDEX tracker_field_values_text_idx ON tracker_field_values (field_id, text) WHERE hidden = 0;         -- f.<slug> text, url and email filters
CREATE INDEX tracker_field_values_at_idx ON tracker_field_values (field_id, at) WHERE hidden = 0;             -- f.<slug> date filters and their earliest/latest totals
CREATE INDEX tracker_field_values_ref_idx ON tracker_field_values (field_id, ref) WHERE hidden = 0;           -- f.<slug> dropdown, labels, relationship and people filters

-- Authored on one end and DERIVED on the other, which is what `derived` says.
-- The one-sided flags are the repair duty's whole input.
CREATE TABLE tracker_relations (
    task_id         TEXT    NOT NULL,
    other_id        TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    derived         INTEGER NOT NULL DEFAULT 0,
    one_sided       INTEGER NOT NULL DEFAULT 0,
    one_sided_final INTEGER NOT NULL DEFAULT 0,
    note            TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (task_id, other_id, kind)
);
CREATE INDEX tracker_relations_other_idx ON tracker_relations (other_id, kind);                               -- the reverse edge: duplicated_by, blocking, linked_page
CREATE INDEX tracker_relations_repair_idx ON tracker_relations (task_id)
    WHERE one_sided = 1 AND one_sided_final = 0;                                                              -- the relations repair duty's selection

-- The dependency edge, derived from the waiting_on relations, and the table the
-- two deleted task columns are now computed from. It carries its own partial
-- indexes for exactly the two predicates those columns used to serve.
CREATE TABLE tracker_task_deps (
    blocker_id    TEXT    NOT NULL,
    task_id       TEXT    NOT NULL,
    blocker_open  INTEGER NOT NULL DEFAULT 1,
    -- The EFFECTIVE finish instant of the blocker, so the comparison against
    -- what a dependent was told is between two fleet-agreed values rather than
    -- two writers' clocks — on authored clocks a sixty-second skew re-issues a
    -- late wake for every task cleared inside that window, on every tick.
    cleared_at    INTEGER,
    PRIMARY KEY (blocker_id, task_id)
);
CREATE INDEX tracker_task_deps_task_idx ON tracker_task_deps (task_id, blocker_open);                         -- blocked=true|false, has_dependencies=, get_task's blocked_by[]
CREATE INDEX tracker_task_deps_open_idx ON tracker_task_deps (task_id) WHERE blocker_open = 1;                -- preset=blocked, and preset=my_queue's blocked=false
CREATE INDEX tracker_task_deps_cleared_idx ON tracker_task_deps (task_id)
    WHERE cleared_at IS NOT NULL;                                                                             -- the missed-unblocked repair duty

-- Derived on every task and comment apply from the keys their text mentions,
-- resolved through the key directory. A removal deletes nothing here, because a
-- removal deletes nothing at all: a restore at any age answers what it did.
CREATE TABLE tracker_references (
    to_task      TEXT NOT NULL,
    from_task    TEXT NOT NULL,
    from_comment TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (to_task, from_task, from_comment)
);
CREATE INDEX tracker_references_from_idx ON tracker_references (from_task);                                   -- references=<key>, and the purge's outbound delete

CREATE TABLE tracker_checklist_items (
    task_id      TEXT    NOT NULL,
    checklist_id TEXT    NOT NULL,
    item_id      TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    done         INTEGER NOT NULL DEFAULT 0,
    assignee     TEXT    NOT NULL DEFAULT '',
    parent_id    TEXT,
    ord          INTEGER NOT NULL DEFAULT 0,
    promoted_to  TEXT,
    PRIMARY KEY (task_id, checklist_id, item_id)
);
CREATE INDEX tracker_checklist_items_assignee_idx ON tracker_checklist_items (assignee, done)
    WHERE assignee <> '';                                                                                     -- checklist_assignee= filter, and the checklist wake
CREATE INDEX tracker_checklist_items_progress_idx ON tracker_checklist_items (task_id, done);                 -- include=checklist_progress

CREATE TABLE tracker_goal_owners (
    goal_id TEXT    NOT NULL,
    handle  TEXT    NOT NULL,
    member  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (goal_id, handle)
);
CREATE INDEX tracker_goal_owners_handle_idx ON tracker_goal_owners (handle);                                  -- a person's own goals, and the goal_owner wake's recipients

CREATE TABLE tracker_goal_targets (
    goal_id   TEXT    NOT NULL,
    target_id TEXT    NOT NULL,
    name      TEXT    NOT NULL,
    type      TEXT    NOT NULL,
    start     REAL    NOT NULL DEFAULT 0,
    goal      REAL    NOT NULL DEFAULT 0,
    current   REAL    NOT NULL DEFAULT 0,
    unit      TEXT    NOT NULL DEFAULT '',
    done      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (goal_id, target_id)
);

-- One row per referenced task OR project, which is why it carries a kind: a
-- target names both, and two tables for one relation is two things to keep in
-- step.
CREATE TABLE tracker_goal_target_refs (
    goal_id   TEXT NOT NULL,
    target_id TEXT NOT NULL,
    kind      TEXT NOT NULL,
    ref       TEXT NOT NULL,
    PRIMARY KEY (goal_id, target_id, kind, ref)
);
CREATE INDEX tracker_goal_target_refs_ref_idx ON tracker_goal_target_refs (kind, ref);                        -- goal=<id> on a task, and a task target's own progress

-- ---------------------------------------------------------------------------
-- The history tables. `notified` is a live column and not a vestige: a quiet
-- commit writes a history row like every other, and `notified` is how a reader
-- tells "nothing was announced" from "nothing happened".
--
-- Every persisted position is a (stream, generation, seq) TRIPLE. `log_seq` is
-- the composed integer and the two companions are what let a woken node refuse
-- a position from a dead sequence space rather than comparing it.
-- ---------------------------------------------------------------------------

CREATE TABLE tracker_history (
    id            TEXT    NOT NULL PRIMARY KEY,
    subject_kind  TEXT    NOT NULL,
    subject_id    TEXT    NOT NULL,
    -- Written by the applier from the task row it already reads, so the
    -- activity query's own gate is an index range rather than a join.
    project_key   TEXT    NOT NULL DEFAULT '',
    kind          TEXT    NOT NULL,
    actor         TEXT    NOT NULL DEFAULT '',
    actor_kind    TEXT    NOT NULL DEFAULT '',
    operator_id   TEXT    NOT NULL DEFAULT '',
    comment_id    TEXT    NOT NULL DEFAULT '',
    batch_id      TEXT,
    excerpt       TEXT    NOT NULL DEFAULT '',
    fields_json   TEXT    NOT NULL DEFAULT '{}',
    turn_id       TEXT    NOT NULL DEFAULT '',
    notified      INTEGER NOT NULL DEFAULT 0,
    late          INTEGER NOT NULL DEFAULT 0,
    log_seq        INTEGER NOT NULL,
    log_stream     TEXT    NOT NULL,
    log_generation INTEGER NOT NULL DEFAULT 0,
    -- created_at is the AUTHORED instant; effective_at the fleet-agreed one;
    -- and broker_at the RAW value effective_at is a MAX over. The raw value is
    -- stored because a lossy one cannot be recomputed — without this column no
    -- repair tool can ever exist, since the only other durable copy is the
    -- deferred row a reprocess deletes.
    created_at     INTEGER NOT NULL,
    broker_at      INTEGER NOT NULL DEFAULT 0,
    effective_at   INTEGER NOT NULL DEFAULT 0,
    skew_ms        INTEGER NOT NULL DEFAULT 0,
    document       BLOB    NOT NULL
);
CREATE INDEX tracker_history_subject_idx ON tracker_history (subject_id, log_seq DESC);                       -- work_activity(task:), and get_task's own feed
CREATE INDEX tracker_history_project_idx ON tracker_history (project_key, log_seq DESC);                      -- work_activity(container=project:) with its since: bound
CREATE INDEX tracker_history_kind_idx ON tracker_history (kind, effective_at);                                -- every report's window predicate, on the instant a duration uses
CREATE INDEX tracker_history_comment_idx ON tracker_history (comment_id);                                     -- resolving a comment back to the commit that wrote it
CREATE INDEX tracker_history_batch_idx ON tracker_history (batch_id);                                         -- batch= (what one bulk call did)
CREATE INDEX tracker_history_seq_idx ON tracker_history (log_seq);                                            -- the activity feed's since: keyset, which is a position
CREATE INDEX tracker_history_kind_seq_idx ON tracker_history (kind, log_seq);                                  -- the missed-unblocked repair, which is bounded by its own last position and never by a clock

CREATE TABLE tracker_turns (
    id             TEXT    NOT NULL PRIMARY KEY,
    task_id        TEXT    NOT NULL,
    seat           TEXT    NOT NULL DEFAULT '',
    turn_id        TEXT    NOT NULL DEFAULT '',
    trigger        TEXT    NOT NULL DEFAULT '',
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_read     INTEGER NOT NULL DEFAULT 0,
    cache_write    INTEGER NOT NULL DEFAULT 0,
    phases_json    TEXT    NOT NULL DEFAULT '[]',
    rounds         INTEGER NOT NULL DEFAULT 0,
    wall_ms        INTEGER NOT NULL DEFAULT 0,
    outcome        TEXT    NOT NULL DEFAULT '',
    log_seq        INTEGER NOT NULL,
    log_stream     TEXT    NOT NULL,
    log_generation INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL,
    -- Turn rows are IN the effective-instant chain, so they carry the raw
    -- broker value for the same reason the history rows do.
    broker_at      INTEGER NOT NULL DEFAULT 0,
    effective_at   INTEGER NOT NULL DEFAULT 0,
    skew_ms        INTEGER NOT NULL DEFAULT 0,
    document       BLOB    NOT NULL
);
-- The table carried no index at all on the ordering the closed form reads, so
-- the effective-instant clamp never saw a turn row.
CREATE INDEX tracker_turns_task_seq_idx ON tracker_turns (task_id, log_seq DESC);                             -- the effective-instant closed form, and get_task's spend
CREATE INDEX tracker_turns_task_effective_idx ON tracker_turns (task_id, effective_at);                       -- a task's turns inside a report window
CREATE INDEX tracker_turns_seq_idx ON tracker_turns (log_seq);                                                -- the spend feed's own position keyset

-- Recomputed WHOLESALE from the task's history rows sorted by the composed
-- position, with both endpoints taken from the effective instant — which is
-- what makes a late reprocess and a redelivery both no-ops AND makes a
-- duration non-negative.
CREATE TABLE tracker_status_spans (
    task_id       TEXT    NOT NULL,
    record_id     TEXT    NOT NULL,
    status        TEXT    NOT NULL,
    grp           TEXT    NOT NULL,
    entered_at    INTEGER NOT NULL,
    left_at       INTEGER,
    project_key   TEXT    NOT NULL DEFAULT '',
    sprint_number INTEGER,
    PRIMARY KEY (task_id, record_id)
);
CREATE INDEX tracker_status_spans_group_idx ON tracker_status_spans (grp, entered_at);                        -- cycle_time and lead_time over a window
CREATE INDEX tracker_status_spans_sprint_idx ON tracker_status_spans (project_key, sprint_number, grp);       -- the burndown and burnup
CREATE INDEX tracker_status_spans_status_idx
    ON tracker_status_spans (project_key, sprint_number, grp, status, entered_at);                            -- the cumulative flow report

-- Divergent: written only by an applied commit and TRAVELS inside a snapshot,
-- but excluded from the identity claim, because the applier applies the inbox
-- horizon from the epoch it was given and two nodes briefly on different epochs
-- legitimately write different rows.
CREATE TABLE tracker_notifications (
    record_id      TEXT    NOT NULL,
    recipient      TEXT    NOT NULL,
    subject_id     TEXT    NOT NULL,
    subject_key    TEXT    NOT NULL DEFAULT '',
    kind           TEXT    NOT NULL,
    reason         TEXT    NOT NULL,
    addressed      INTEGER NOT NULL DEFAULT 0,
    fallback_only  INTEGER NOT NULL DEFAULT 0,
    fallback_rank  INTEGER NOT NULL DEFAULT 0,
    excerpt        TEXT    NOT NULL DEFAULT '',
    -- AUTHORED, because an inbox renders an instant rather than measuring a
    -- duration.
    created_at     INTEGER NOT NULL,
    log_seq        INTEGER NOT NULL,
    log_stream     TEXT    NOT NULL,
    log_generation INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (record_id, recipient)
);
CREATE INDEX tracker_notifications_inbox_idx ON tracker_notifications (recipient, log_seq DESC);              -- work_inbox
CREATE INDEX tracker_notifications_reason_idx ON tracker_notifications (recipient, reason, log_seq DESC);     -- work_inbox's primary/other split
CREATE INDEX tracker_notifications_swept_idx ON tracker_notifications (created_at);                           -- the per-node inbox retention sweep, which is a range delete

-- ---------------------------------------------------------------------------
-- The domain's own machinery: the operation ledger, the deferred records and
-- their scope index. All three are `Local` — scrubbed from a donated snapshot
-- — and all three live in THIS file rather than the node's own, because
-- contract 2 puts them in the applier's transaction and a transaction is one
-- file.
-- ---------------------------------------------------------------------------

-- THE SHAPE OF ALL THREE IS THE FRAMEWORK'S, not this domain's: the framework
-- writes them, and it writes the same statements for every domain it carries.
-- The migration that landed the framework documents the shape a domain's own
-- two tables must have, and this file is written from it — a column named
-- differently here is a statement that fails at runtime on the one path
-- nothing else exercises.

CREATE TABLE tracker_ops (
    op_id      TEXT    NOT NULL PRIMARY KEY,
    subject    TEXT    NOT NULL,
    -- The COMPOSED (generation << 40) | sequence, so a position from before a
    -- reanchor is comparable and safely stale rather than plausible.
    position   INTEGER NOT NULL,
    applied_at INTEGER NOT NULL
);
-- The sweep is a range delete over the age, and a range delete ships its index:
-- without it a node returning from a month away scans the whole table on every
-- tick, and deletes its whole month in one statement holding the only writer.
CREATE INDEX tracker_ops_swept_idx ON tracker_ops (applied_at);                                               -- the ops retention sweep

CREATE TABLE tracker_log_deferred (
    position     INTEGER NOT NULL PRIMARY KEY,
    subject      TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    subject_id   TEXT    NOT NULL,
    -- The record version this build could not decode, which is the number an
    -- operator needs to pick a build.
    version      INTEGER NOT NULL,
    -- THE WHOLE MESSAGE, BYTE FOR BYTE. Lossless means the bytes: a build that
    -- can read this record must get exactly what its writer published, not
    -- what an intermediate build understood of it.
    payload      BLOB    NOT NULL,
    -- The broker's own instant, because a record reprocessed after an upgrade
    -- must contribute the instant it was COMMITTED rather than the instant it
    -- was reprocessed — and this row is the only other durable copy of it.
    stored_at    INTEGER NOT NULL
);
-- subject_id LEADS, because the spend probe reads it ALONE: a turn commit and
-- its task's create share an id, and the dependency is on the OBJECT rather
-- than on the kind.
CREATE INDEX tracker_log_deferred_subject_idx ON tracker_log_deferred (subject_id, subject_kind);             -- the applier's own per-object deferral probe

-- One row per scope PATH of the deferred record, written in the SAME
-- transaction as its parent and the checkpoint advance — so there is no window
-- in which the position moved and the scope is unindexed.
--
-- A PATH RATHER THAN A TUPLE OF COORDINATES, because the framework's
-- containment model is a path hierarchy: it knows one thing about a scope, that
-- levels are separated left to right, and it computes coverage from that alone.
-- A tuple with an empty coordinate meaning ANY is a second containment model,
-- and the probe would have to translate between them on every read — which is
-- itself the likeliest place for the rule to be wrong, and one no test of
-- either model would catch.
--
-- The probe is two clauses over this column and neither finds the other's case:
-- a stored path IN the query's closure is one that CONTAINS what the query is
-- about, and a stored path UNDER one of its roots is one INSIDE it.
--
-- A PLAIN ROWID TABLE: `WITHOUT ROWID` is refused by the pinned driver as an
-- experimental feature (measured), so a migration carrying it would fail on
-- every node and every store would refuse to open.
--
-- NO FOREIGN KEY: the parent's own delete removes these rows in the same
-- statement list, because a cascade is a delete nobody committed.
CREATE TABLE tracker_log_deferred_scope (
    position INTEGER NOT NULL,
    path     TEXT    NOT NULL,
    PRIMARY KEY (position, path)
);
CREATE INDEX tracker_log_deferred_scope_idx ON tracker_log_deferred_scope (path, position);                   -- the read barrier's coverage probe and the writer's step 0

-- ---------------------------------------------------------------------------
-- Three object tables that are the fleet's own record rather than a person's.
-- ---------------------------------------------------------------------------

-- The row that holds the rank order's own version, which is what the broker
-- expectation on that subject is formed from. Fifty rows on a mature company.
CREATE TABLE tracker_rank_orders (
    project_key    TEXT    NOT NULL PRIMARY KEY,
    version        INTEGER NOT NULL,
    scoped_through INTEGER
);

-- Every reanchor is a committed record and therefore reproducible from the log;
-- this is what a recovery replay writes and what the operator surface reads to
-- say "generation 2, reanchored by … at …". Without it a reanchor is an
-- operator action with no durable trace on the estate it changed.
CREATE TABLE tracker_log_generations (
    generation             INTEGER NOT NULL PRIMARY KEY,
    at                     INTEGER NOT NULL,
    by                     TEXT    NOT NULL DEFAULT '',
    prev_stream_created_at INTEGER NOT NULL DEFAULT 0,
    new_stream_created_at  INTEGER NOT NULL DEFAULT 0,
    -- Both load-bearing for the audit trail: "what was the old stream's head
    -- when we walked away from it, and why did we" is the whole content of a
    -- reanchor's record.
    prev_last_seq_seen     INTEGER NOT NULL DEFAULT 0,
    reason                 TEXT    NOT NULL DEFAULT '',
    record_id              TEXT    NOT NULL DEFAULT ''
);

-- ONE ROW PER STREAM, because the gate compares a position against a record's
-- own and positions on different streams do not compare. Both positions are
-- COMPOSED, so the generation rides in their high bits and the table needs no
-- generation column. A readmission is the INVERSE COMMIT rather than a delete,
-- so an eviction's whole history survives a replay.
CREATE TABLE tracker_evictions (
    node_id             TEXT    NOT NULL,
    log_stream          TEXT    NOT NULL,
    from_position       INTEGER NOT NULL,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    readmitted_position INTEGER,
    readmitted_at       INTEGER,
    PRIMARY KEY (node_id, log_stream)
);

-- The deletion marker: what a purge removed, so a node that was away can tell a
-- task that never existed from one that was deliberately destroyed.
CREATE TABLE tracker_deletions (
    task_id       TEXT    NOT NULL PRIMARY KEY,
    task_key      TEXT    NOT NULL,
    project_key   TEXT    NOT NULL,
    by            TEXT    NOT NULL,
    by_kind       TEXT    NOT NULL,
    reason        TEXT    NOT NULL DEFAULT '',
    committed_seq INTEGER NOT NULL,
    log_stream    TEXT    NOT NULL,
    log_generation INTEGER NOT NULL DEFAULT 0,
    at            INTEGER NOT NULL,
    document      BLOB    NOT NULL
);
CREATE INDEX tracker_deletions_project_idx ON tracker_deletions (project_key, at DESC);                       -- work_retention's deletions block, and work_trash's purged half
CREATE INDEX tracker_deletions_key_idx ON tracker_deletions (task_key);                                       -- resolving a key that no longer exists to why it does not
