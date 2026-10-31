-- A room says what changed in it, and a badge can tell a mention from a remark.
--
-- WHO HAS TO AGREE ON IT: every data node, identically. Each column below is a
-- column of the chat domain's own replicated rows, written by its applier and
-- nothing else.
--
-- # What was missing
--
-- A room changed silently. Who joined it, who put somebody in it or took
-- them out, and what its topic and description were set to lived only in the
-- room's current state, so a person opening a room could not see that a
-- colleague had just been added, or that the description they were reading
-- had been rewritten an hour ago. The applier now writes each such change as
-- a ROOM EVENT: a row of `chat_messages`, numbered in the room's own sequence
-- like a message, so the transcript draws it where it happened and a page, a
-- cursor and a live frame carry it with no second table to merge. `event` is
-- its kind (`joined`, `added`, `left`, `removed`, `description`, `topic`,
-- `template`), and empty on every message somebody wrote. The rail, the
-- unread count, the mention count and the search index all read `event = ''`,
-- because a room event is not conversation: it is not a badge somebody can
-- clear by reading, and it is not words anybody said.
--
-- And the rail could not tell a person how many of the messages waiting for
-- them named them. A mention of a handle was already a row (`chat_mentions`),
-- but a message that addressed the whole room (`@channel`) said so only inside
-- its document, which a count over a room's newest hundred messages would have
-- had to decode one by one on every poll. `collective` is that flag as a
-- column, so the count is one statement.
--
-- And the activity feed could not find the reactions to one person's
-- messages without walking every reaction in the company: a reaction row named
-- the message and the reactor, not whose message it was. `message_author` is
-- the reacted-to message's author, written beside the reaction, and the index
-- under it is the feed's own order.
--
-- And a room could not take in the people a company hires. An administrator
-- who wants every new agent and person in `#general` had to add each one by
-- hand. `chat_channels.auto_join` is the room's setting, and `chat_autojoin`
-- is what makes it "from now on" rather than "everybody": the seats the
-- setting has already accounted for, which are the company's seats when it was
-- turned on (nobody already in the company is added) and every seat it put in
-- the room since. A seat in the org chart that the room has not accounted for
-- is a seat that joined the company after the setting, and the engine's
-- reconcile puts it in the room once, by a membership record that also
-- accounts for it, so a seat that later leaves the room is not put back. A row
-- per seat rather than a list in the room's document, because the set grows
-- with the company and a room's document is read on every rail.
--
-- # Written by the apply, re-derived by the applier, never by SQL here
--
-- The apply writes all four from the record it applies. The chat applier's
-- own Rederive (internal/chatlet, `statelog.Deriver`) recomputes them, in the
-- same Go that decodes a message's document for the apply, whenever a node's
-- checkpoint names another rule set than its build derives at: rows an older
-- build wrote, a checkpoint a reanchor created, or rows adopted from a build
-- whose rules differ. So this migration adds the columns, defaulted, and
-- nothing else: an UPDATE here would be a second implementation of what a
-- message's flag is, and two implementations of one rule are two answers the
-- day either is edited (0042 says the same of an invitation's seat).
--
-- # Why no index on `event` or `collective`
--
-- Neither is a predicate a read SEEKS on. Every read that filters on them is
-- already driven by `chat_messages_channel_idx` over one room's newest rows,
-- and evaluates the flag on the rows that index yields: an index over a column
-- whose value is the same on nearly every row is a second copy of the table's
-- order that no plan would choose.
ALTER TABLE chat_messages ADD COLUMN event TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_messages ADD COLUMN collective INTEGER NOT NULL DEFAULT 0;

ALTER TABLE chat_reactions ADD COLUMN message_author TEXT NOT NULL DEFAULT '';
CREATE INDEX chat_reactions_author_idx ON chat_reactions (message_author, at DESC);       -- the reactions to one person's messages

ALTER TABLE chat_channels ADD COLUMN auto_join INTEGER NOT NULL DEFAULT 0;
CREATE INDEX chat_channels_auto_join_idx ON chat_channels (id) WHERE auto_join = 1;       -- the rooms the reconcile staffs

CREATE TABLE chat_autojoin (
    channel_id TEXT    NOT NULL,
    handle     TEXT    NOT NULL,
    -- The broker's instant of the record that accounted for this seat.
    at         INTEGER NOT NULL,
    version    INTEGER NOT NULL,
    PRIMARY KEY (channel_id, handle)
);
