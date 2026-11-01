-- A change is discussed on its own subject.
--
-- What is said ABOUT a change (web!42) — who is asked to review it, its
-- comments and the threads they form — is state on the code log like the change
-- itself, written by records on a subject of the CHANGE's own
-- (crewlet.code.log.change.<id>), never on its repository's. A change's
-- lifecycle is decided by its repository's one committer, in the queue its
-- pushes wait in (0081); a comment shares nothing with those pushes, and
-- decided there it would wait behind their batches. So these four tables are
-- written by the records on the change's subject, which the repository's
-- committing data node publishes, and by nothing on the repository's: a table
-- is written by the records of one subject only, or two writers deciding from
-- two snapshots would contend on one row.
--
-- code_conversations is one row a change that has been discussed, made by its
-- first record and moved by every record after it.
--   - repo_id is the change's repository, as the record that made the row named
--     it — the one every record on the subject names, which the applier holds
--     to code_changes.repo_id.
--   - comments counts the change's comments.
--   - text_bytes is what the conversation's WIDE text weighs — every comment's
--     body — so a purge weighs a change's conversation by this one narrow row
--     and never reads a comment to learn what deleting it costs (0089's rule for
--     a change's body). Every record that writes such text adds what it wrote,
--     and a purge part that cuts a change's comments takes what it deleted
--     back off.
--   - version counts the records on the change's subject, and position is the
--     packed position of the last: a redelivered record at or below it applies
--     as nothing, and a writer answers a record applied once the row is at it.
--
-- code_reviewers is one row each reviewer a change has asked, by handle: a seat
-- (reviewer_kind agent) or a person by the human seat they hold (person),
-- stamped from the chart by the writer. reason is why they were asked
-- (requested: by name), state whether the ask stands (requested) or was
-- withdrawn (withdrawn) — kept, so the change says who was once asked —
-- requested_by and requested_by_kind who asked, note what they asked for, at
-- most 1 KiB, and position the record that last moved the row.
--
-- code_participants is everybody who takes part in a change's conversation:
-- each reviewer asked and each author of a comment, by handle and kind, from
-- the record that first named them.
--
-- code_comments is one row a comment. thread_id is the thread it is in: its own
-- id for the first comment of a thread, the thread's root for a reply, since a
-- thread is answered at its root (parent_id, empty on a root). author,
-- author_kind and author_operator_id are who said it — a seat, or a person with
-- the credential they used — anchor the line a root points at (JSON, empty for
-- none), resolved, resolved_by and resolved_by_kind a root's thread resolved and
-- by whom, turn_id the turn that said it, position and at its record's packed
-- position and broker instant. body_bytes is the body's width, which a purge
-- part's cut reads off the rows it deletes. THE BODY IS DECLARED LAST: a
-- comment is up to 32 KiB, and a row read for a column before it — a thread's
-- root, read to resolve the thread — then stops at its leaf page (0089's
-- measurement, declared last: a third off the read).
--
-- NO repo_id ON THE REVIEWERS, THE PARTICIPANTS OR THE COMMENTS: each is
-- reached through its change, by its key, and a purge reaches them through
-- code_changes_number_idx, as it does a change's revisions and authors (0081).
--
-- # The indexes
--
-- code_comments_thread_idx is a change's comments in thread order: what a
-- change's timeline reads, and the order a purge part's cut takes a change's
-- comments in.
--
-- code_reviewers_reviewer_idx is a reviewer's asks by whether they stand,
-- newest change first: the listing of the changes waiting on one reviewer
-- (reviewer=, needs=review), which reads the standing asks and none of the
-- withdrawn ones. Every other read of code_reviewers is one change's, and the
-- one driven from a list of reviewers NAMES the key: beside this index it was
-- planned as a seek of the reviewer alone, every ask of that reviewer on every
-- change read to find the one.
--
-- Class: code_conversations, code_reviewers, code_participants and
-- code_comments are reproducible: written only by the applier, from the records
-- on a change's own subject, and removed only by its repository's purge, a
-- change's conversation before its row.
CREATE TABLE code_conversations (
    change_id  TEXT    NOT NULL PRIMARY KEY,
    repo_id    TEXT    NOT NULL,
    comments   INTEGER NOT NULL,
    text_bytes INTEGER NOT NULL,
    version    INTEGER NOT NULL,
    position   INTEGER NOT NULL
);

CREATE TABLE code_reviewers (
    change_id         TEXT    NOT NULL,
    reviewer          TEXT    NOT NULL,
    reviewer_kind     TEXT    NOT NULL,
    reason            TEXT    NOT NULL,
    state             TEXT    NOT NULL,
    requested_by      TEXT    NOT NULL,
    requested_by_kind TEXT    NOT NULL,
    note              TEXT    NOT NULL DEFAULT '',
    position          INTEGER NOT NULL,
    PRIMARY KEY (change_id, reviewer)
);

CREATE TABLE code_participants (
    change_id TEXT    NOT NULL,
    handle    TEXT    NOT NULL,
    kind      TEXT    NOT NULL,
    position  INTEGER NOT NULL,
    PRIMARY KEY (change_id, handle)
);

CREATE TABLE code_comments (
    id                 TEXT    NOT NULL PRIMARY KEY,
    change_id          TEXT    NOT NULL,
    thread_id          TEXT    NOT NULL,
    parent_id          TEXT    NOT NULL DEFAULT '',
    author             TEXT    NOT NULL,
    author_kind        TEXT    NOT NULL,
    author_operator_id TEXT    NOT NULL DEFAULT '',
    anchor             TEXT    NOT NULL DEFAULT '',
    resolved           INTEGER NOT NULL DEFAULT 0,
    resolved_by        TEXT    NOT NULL DEFAULT '',
    resolved_by_kind   TEXT    NOT NULL DEFAULT '',
    turn_id            TEXT    NOT NULL DEFAULT '',
    body_bytes         INTEGER NOT NULL,
    position           INTEGER NOT NULL,
    at                 INTEGER NOT NULL,
    body               TEXT    NOT NULL
);

CREATE INDEX code_reviewers_reviewer_idx ON code_reviewers (reviewer, state, change_id);    -- the changes waiting on one reviewer, newest first: reviewer= and needs=review

CREATE INDEX code_comments_thread_idx ON code_comments (change_id, thread_id, position);    -- a change's comments in thread order: its timeline, and a purge part's cut of them
