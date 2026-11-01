-- A change is opened on its repository's log.
--
-- A change (web!42) proposes that one branch of a repository, its SOURCE, be
-- merged into another, its TARGET. Like the refs it names, it is state on the
-- code log: three replicated tables the applier writes from the records on the
-- REPOSITORY's subject (crewlet.code.log.repo.<id>), never on a subject of the
-- change's own. That is what puts a change's lifecycle and the pushes that move
-- its branches in front of one committer and one broker arbitration, so a
-- change is never both merged and closed, and what lets its number be read and
-- advanced inside the apply, in log order. What is said ABOUT a change — its
-- reviews, its comments, the facts a git node finds about a revision — is
-- written on a subject of the change's own, into tables of their own: a table
-- is written by the records of one subject only, or two writers would contend
-- on one row from two snapshots.
--
-- code_changes is one row a change.
--   - number is the repository's code_repos.next_change at the apply that opens
--     it, which the same apply advances, so numbers follow the log's order and
--     no two of a repository's changes share one. The schema does not ask that
--     of the rows: a constraint violation inside the apply aborts it on every
--     node at once (0074, NO UNIQUE OUTSIDE A PRIMARY KEY).
--   - state is draft, open, closed or merged (code.ChangeStates). closed_at is
--     set exactly while it is closed and merged_at exactly while it is merged;
--     merged_via is how it was merged (code.MergedVia) and merge_oid the commit
--     its target was merged at.
--   - head_oid is the source branch's head at its latest revision, and revision
--     that revision's n in code_revisions.
--   - owner is who opened it, by handle — a seat, or the human seat a person
--     holds — owner_kind is agent or person, and owner_operator_id the
--     credential a person opened it with.
--   - work_backend, work_id, work_key and work_project name the work item it
--     carries, all empty when it carries none; created_turn and created_run
--     the turn and the sandbox run it was opened in, each empty where there
--     was none — a person's open has neither.
--   - opened_at and opened_position are the opening record's broker instant
--     (store.EncodeTime, unix microseconds) and packed position; version counts
--     the records that changed the row — 1 at its open — which an edit states
--     as its expectation, and position is the packed position of the last.
--
-- code_change_authors is who wrote the change: its owner and every principal
-- whose push moved its head, each from the revision it first wrote. Keyed by
-- the change and the handle, because an author's approval of their own change
-- counts for nothing, and a review's decide asks exactly that of one key.
--
-- code_revisions is one row each time the change's head moved while it was
-- live: n counts from 1, the source branch's head when the change was opened;
-- pusher and pusher_kind are who moved it, and push_id the push whose record
-- did, empty for the first revision of a change a change record opened rather
-- than a push. A revision is a row and never a ref: nothing a client fetches
-- advertises one.
--
-- NO repo_id ON THE AUTHORS OR THE REVISIONS: both are reached through their
-- change, by its key, and a purge reaches them through
-- code_changes_number_idx. A column and an index entry on every revision of
-- every change would be paid for a statement that runs once in a repository's
-- life.
--
-- code_repos.next_change is the number the repository's next change takes.
-- SQLite requires a default for a NOT NULL column added to a table, and here
-- the default is the value every repository has: a repository no change has
-- been opened on numbers its first change 1.
--
-- # The indexes
--
-- PARTIAL ON THE LIVE STATES, `state IN ('draft', 'open')`: a change closed or
-- merged is never what a push, an open or a git node's work asks after, and a
-- repository's history holds far more of those than of live ones. Every
-- statement over the two partial indexes repeats the predicate VERBATIM, the
-- states in this order (internal/code's liveChanges): the pinned planner
-- matches the list as it is written, and with the states reversed, or one of
-- them alone, a statement reads another index.
-- code_changes_open_idx holds at most one row a source branch — a branch heads
-- one live change at a time — and code_changes_target_idx ends in head_oid,
-- since what a push asks it is which live changes have exactly the head the
-- push sets their target to: the target's live changes alone would be read
-- whole on every push to it.
--
-- The listings' indexes END IN THE CHANGE'S ID: one record can open several
-- changes — a push record holds up to 64 pushes, and each may open its own —
-- so a page cursor's order is total only with it.
--
-- Class: code_changes, code_change_authors and code_revisions are
-- reproducible: written only by the applier, from the records on the
-- repository's subject, and removed only by the repository's purge, a change's
-- revisions and authors before the change. code_repos stays reproducible:
-- next_change is written only by the apply that opens a change.
CREATE TABLE code_changes (
    id                TEXT    NOT NULL PRIMARY KEY,
    repo_id           TEXT    NOT NULL,
    number            INTEGER NOT NULL,
    title             TEXT    NOT NULL,
    body              TEXT    NOT NULL DEFAULT '',
    state             TEXT    NOT NULL,
    source_ref        TEXT    NOT NULL,
    target_ref        TEXT    NOT NULL,
    head_oid          TEXT    NOT NULL,
    revision          INTEGER NOT NULL,
    owner             TEXT    NOT NULL,
    owner_kind        TEXT    NOT NULL,
    owner_operator_id TEXT    NOT NULL DEFAULT '',
    work_backend      TEXT    NOT NULL DEFAULT '',
    work_id           TEXT    NOT NULL DEFAULT '',
    work_key          TEXT    NOT NULL DEFAULT '',
    work_project      TEXT    NOT NULL DEFAULT '',
    created_turn      TEXT    NOT NULL DEFAULT '',
    created_run       TEXT    NOT NULL DEFAULT '',
    opened_at         INTEGER NOT NULL,
    opened_position   INTEGER NOT NULL,
    closed_at         INTEGER,
    close_reason      TEXT    NOT NULL DEFAULT '',
    merged_at         INTEGER,
    merged_via        TEXT    NOT NULL DEFAULT '',
    merge_oid         TEXT    NOT NULL DEFAULT '',
    version           INTEGER NOT NULL,
    position          INTEGER NOT NULL
);

CREATE INDEX code_changes_number_idx ON code_changes (repo_id, number);    -- a change by its number (web!42), a repository's changes in number order, and a purge's walk to each change's revisions and authors
CREATE INDEX code_changes_open_idx ON code_changes (repo_id, source_ref) WHERE state IN ('draft', 'open');    -- the live change a source branch heads: an open's decide, the push apply's revisions and closes, the ref expiry's exemption, a run's delivered refs and the home's due listing
CREATE INDEX code_changes_target_idx ON code_changes (repo_id, target_ref, head_oid) WHERE state IN ('draft', 'open');    -- the live changes into a target: the push apply's merge of a change whose head its target is pushed to, and the home's recheck when a target moves
CREATE INDEX code_changes_state_idx ON code_changes (state, opened_position, id);    -- the company's changes in a state, newest first
CREATE INDEX code_changes_owner_idx ON code_changes (owner, state, opened_position, id);    -- an owner's changes in a state, newest first
CREATE INDEX code_changes_work_idx ON code_changes (work_backend, work_id) WHERE work_id != '';    -- a work item's changes
CREATE INDEX code_changes_merge_idx ON code_changes (repo_id, merge_oid) WHERE merge_oid != '';    -- the change a commit merged: a commit's page names it

CREATE TABLE code_change_authors (
    change_id      TEXT    NOT NULL,
    author         TEXT    NOT NULL,
    author_kind    TEXT    NOT NULL,
    first_revision INTEGER NOT NULL,
    position       INTEGER NOT NULL,
    PRIMARY KEY (change_id, author)
);

CREATE TABLE code_revisions (
    change_id   TEXT    NOT NULL,
    n           INTEGER NOT NULL,
    head_oid    TEXT    NOT NULL,
    pusher      TEXT    NOT NULL,
    pusher_kind TEXT    NOT NULL,
    push_id     TEXT    NOT NULL DEFAULT '',
    position    INTEGER NOT NULL,
    at          INTEGER NOT NULL,
    PRIMARY KEY (change_id, n)
);

CREATE INDEX code_revisions_head_idx ON code_revisions (head_oid);    -- the revisions a commit heads: a commit's page names "revision n of web!42"

ALTER TABLE code_repos ADD COLUMN next_change INTEGER NOT NULL DEFAULT 1;
