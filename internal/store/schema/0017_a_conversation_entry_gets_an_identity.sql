-- A conversation entry gets a name of its own.
--
-- Every other table a seat's memory lives in already has one — agent_diary.id,
-- episodes.id, synthesized_skills.id are all written by whoever created the
-- row. conversation_sessions is the exception: its `id` is an AUTOINCREMENT,
-- which is a NODE-LOCAL name. It starts at 1 on every node, so two unrelated
-- entries written on two nodes both call themselves 5. That is harmless while
-- a seat's memory never leaves the node that wrote it — and it is exactly what
-- stops that memory from following the seat when placement moves it, because
-- there is no name a peer could carry the row under.
--
-- entry_id is that name. Written once, by the node that created the row, and
-- unique across the fleet because a seat is held by ONE node at a time: no two
-- nodes ever independently create the same entry, so there is nothing for a
-- content hash to collapse and no coordination to arrange.
--
-- IT IS NOT A SECOND DEDUPE, and must not be confused with one.
-- conversation_sessions_work_key_idx is the dedupe: one entry per work key,
-- PARTIAL over `work_key <> ''` because '' is the documented "a turn with no
-- ledgerable trigger" and those turns are legitimately distinct rows that must
-- never collide. entry_id is total precisely because it says nothing about
-- what a row means — two unkeyed turns in one conversation get two ids and
-- stay two rows, which is the behaviour the partial index exists to protect.
--
-- `id` stays. It is the node-local insertion sequence, and both the
-- trim-on-write and the newest-first read order use it to break a tie between
-- two entries stamped the same millisecond.
ALTER TABLE conversation_sessions ADD COLUMN entry_id TEXT NOT NULL DEFAULT '';

-- Rows written before this migration have no cross-node identity to preserve:
-- they were written when a seat's memory could not travel, so they exist only
-- on the node that wrote them and have no twin elsewhere to be reconciled
-- with. A random name per row is therefore exact, and it is what lets the
-- index below be total rather than partial.
UPDATE conversation_sessions
   SET entry_id = lower(hex(randomblob(16)))
 WHERE entry_id = '';

CREATE UNIQUE INDEX conversation_sessions_entry_id_idx
    ON conversation_sessions (entry_id);
