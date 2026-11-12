-- A change is listed by when it last moved.
--
-- A listing of changes (Code › Changes) says when each one last moved, and is
-- narrowed to the changes that moved within a window (`window=24h|7d|30d`): the
-- question a person opening the screen after a day away asks first. A change
-- moves on TWO subjects (0086, 0100): its lifecycle and the pushes that move its
-- head on its repository's, what is said about it on its own. A table is
-- written by the records of one subject only, so the instant is kept twice, once
-- a subject, and a change's last move is the later of the two.
--
-- code_changes.updated_at is the broker instant (store.EncodeTime, unix
-- microseconds) of the last record on the repository's subject that changed
-- the row — the one that moved its version: its open, an edit, a draft, a
-- retarget, a close, a reopen, a landing, a merge, and every push that moved
-- its head, closed it or merged it.
--
-- code_conversations.updated_at is the broker instant of the last record
-- SOMEBODY WROTE about the change on its own subject — an ask for review, a
-- comment, a thread resolved, a review, an answer to a finding — and 0 for a
-- conversation nobody has spoken in. The home's facts (a revision's facts, a
-- mergeability) and the engine's settling of the asks move the conversation's
-- version but not this: a target branch that moves re-checks every live change
-- into it, and a change listed as moved because its target did would make the
-- window list every change of a busy branch every day.
--
-- SQLite requires a default for a NOT NULL column added to a table, and 0 is
-- "never": no build that wrote a change before this one has shipped, and every
-- row the applier writes from here stamps its own.
--
-- # The indexes
--
-- A window walks the two in step, newest first, and lists each change once, at
-- the later of its two instants (internal/gitlet changeread.go's window walk):
-- each index is sought from the window's start, a page of each, and a change
-- whose other instant is the later is left to the walk of the other index.
-- code_changes_updated_idx ends in the change's id, and
-- code_conversations_updated_idx in the change's key, so a page cursor's order
-- is total.
--
-- code_conversations_updated_idx is PARTIAL on the conversations somebody spoke
-- in: a conversation only the home's facts wrote is never in a window by it,
-- and the statements over it repeat the predicate verbatim.
--
-- WHAT THEY COST. Every write that moves a change's version names updated_at,
-- so it rewrites the change's entry in code_changes_updated_idx — one entry
-- beside the one a head move already rewrote (code_changes_target_idx), so a
-- push record that moves 1 300 changes' heads rewrites 2 600 entries where it
-- rewrote 1 300 (TestAChangeWriteRewritesOnlyTheIndexesItMoves holds each kind
-- of write to it). A conversation record somebody wrote rewrites its change's
-- entry in code_conversations_updated_idx; the home's facts write none.
--
-- Class: code_changes and code_conversations stay REPRODUCIBLE: each instant is
-- written only by the applier, from the record it applies, and an index adds no
-- row.

ALTER TABLE code_changes ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;

ALTER TABLE code_conversations ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;

CREATE INDEX code_changes_updated_idx ON code_changes (updated_at, id);    -- the changes whose lifecycle last moved within a window, newest first: a listing narrowed by window=

CREATE INDEX code_conversations_updated_idx ON code_conversations (updated_at, change_id)
    WHERE updated_at > 0;    -- the changes somebody last spoke about within a window, newest first: a listing narrowed by window=
