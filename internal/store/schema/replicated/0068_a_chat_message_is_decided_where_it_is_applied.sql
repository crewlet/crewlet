-- A chat message's record is decided where it is applied, and says so.
--
-- # chat_verdicts
--
-- 0055's header describes a message as ADDITIVE: a record that carries no
-- expectation because posts commute. They do not all commute, and the applier
-- 0055 shipped already knew it — it declined the thirty-third distinct emoji on
-- a message, skipped an edit of a tombstone, and wrote a post into a room a
-- concurrent record had archived or taken its author out of — while the writer
-- answered `applied` for every one, a record the applier had dropped included.
-- A record whose effect depends on what came before it is SEQUENCED
-- (statelog.PatternSequenced): the log orders it, the applier decides it at its
-- position, and the applier owes a verdict for every record it consumes, which
-- the writer reads back before it says what the write did. This table is that
-- verdict. ADR-0046 is the decision.
--
-- ONE ROW PER MESSAGE-SUBJECT RECORD — a post, an edit, a delete, a reaction —
-- written in the record's own apply transaction, `applied` or why it was
-- refused (`archived`, `not_member`, `gone`, `deleted`, `reactions_full`), or
-- `repeated` for a second copy of an operation an earlier position already
-- decided, which does nothing. Keyed on the record's packed position, which is
-- the row id: every node writes the same rows in the same order. A row holds no
-- content — an operation id, a room, an instant and a word — so an erase and a
-- prune, which remove what was said, have nothing here to remove.
--
-- READ BY OPERATION: the writer reads its own verdict back by the operation id
-- once its applier has reached the record, a retry the operation ledger
-- answers reads the first copy's the same way, and the applier itself asks
-- whether an operation is already decided before it decides a second copy.
--
-- KEPT FOR THE LEDGER'S HORIZON BY THE BROKER'S CLOCK (statelog.OpsRetention):
-- a retry is answered from the ledger only while its operation is inside the
-- mint horizon, so every read of a verdict happens inside that window, and a
-- second copy of an operation lands inside it too. Pruned in each message
-- record's own transaction, at most a batch at a time, oldest first, from the
-- record's broker instant — never a sweep on a node's own clock, which would
-- make the rows differ between nodes on a domain that claims identity.
CREATE TABLE chat_verdicts (
    -- The record's packed position, and the row id.
    position   INTEGER NOT NULL PRIMARY KEY,
    op_id      TEXT    NOT NULL,
    channel_id TEXT    NOT NULL,
    -- applied, archived, not_member, gone, deleted, reactions_full or
    -- repeated.
    verdict    TEXT    NOT NULL,
    -- The broker's instant for the record, which the prune ranges on.
    at         INTEGER NOT NULL
);
CREATE INDEX chat_verdicts_op_idx ON chat_verdicts (op_id);                                -- a writer, a retry, the feed and a second copy reading an operation's verdict
CREATE INDEX chat_verdicts_at_idx ON chat_verdicts (at);                                   -- the horizon's prune, oldest first
