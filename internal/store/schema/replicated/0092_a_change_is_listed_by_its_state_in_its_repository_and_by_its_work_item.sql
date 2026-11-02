-- A change is listed by its state in its repository and by its work item.
--
-- A listing of changes walks ONE index, the one its filters lead (internal/
-- code's changes read): a repository's changes, a work item's, an owner's in a
-- state, or the company's in a state — newest first, a page of rows at a time,
-- keyset after the last row the page read. 0081 gave the company's and an
-- owner's their indexes (code_changes_state_idx, code_changes_owner_idx) and a
-- repository's its number order (code_changes_number_idx); two listings its
-- indexes cannot serve.
--
-- A REPOSITORY'S CHANGES IN ONE STATE — the open ones of `web`, what a
-- repository's page and a seat's tool ask first. Read through the number
-- index, the listing walked every change the repository ever had, testing the
-- state of each, to answer the few still open — and a change has no horizon
-- of its own: it is kept as long as its repository, so that walk grows with
-- the repository's whole history, a closed or merged change for every open
-- one an agent's day of work leaves behind. code_changes_repo_state_idx is
-- (repo_id, state, number): one repository's changes in one state, in number
-- order — the order the code log opened them in, since a number is the
-- apply's (0081) — so the listing reads exactly the rows it answers, newest
-- first, its cursor a number.
--
-- A WORK ITEM'S CHANGES had an index with no order (0081's (work_backend,
-- work_id)), and a listing is paged: a page newest first sorted every change
-- the item ever carried before it took one, and a recurring item — a
-- schedule's, worked every night — carries a change a night for as long as it
-- exists. Recreated as (work_backend, work_id, opened_position, id): the
-- order code_changes_state_idx and code_changes_owner_idx already keep, the
-- position of the record that opened each change and then its id, since one
-- record can open several (a push record carries up to 64 pushes, each
-- opening its own). Still partial on a change that carries an item, the
-- predicate every reader repeats verbatim.
--
-- What they cost: every column either index holds is one a change is born
-- with and never changes but its state and its work item — repo_id, number,
-- opened_position and id never move — so a head move, the write every push to
-- a change's source makes, rewrites neither. An open adds an entry to the
-- first, and to the second where it carries an item; a close, a merge, a
-- reopen or a draft's state moves its entry in the first; and an edit that
-- sets or clears the work item moves its entry in the second, as it did in
-- 0081's shape.
--
-- Class: code_changes is unchanged; an index carries no rows of its own.
CREATE INDEX code_changes_repo_state_idx ON code_changes (repo_id, state, number);    -- Reader.Read(changes) of one repository in one state: its changes in that state, newest first by number

DROP INDEX code_changes_work_idx;

CREATE INDEX code_changes_work_idx ON code_changes (work_backend, work_id, opened_position, id) WHERE work_id != '';    -- Reader.Read(changes) of one work item: its changes newest first, by the position of the record that opened each and its id
