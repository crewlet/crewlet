-- A message says who answered it: one row per seat a person's post reached,
-- marked when that seat answers.
--
-- # What was missing
--
-- The write path routes every post to a set of seats, each under a reason, and
-- carries that set on the record (`Notify.Recipients`) for the wake. Nothing
-- kept it. A person who posted in a room could not tell whether the message
-- reached any agent, or whether the agent it reached ever answered, and the
-- only way to find out was to post a test message and wait.
--
-- # Two halves, two homes
--
-- WHO A POST REACHED, AND WHY, is a fact about the post: the applier writes it
-- onto the message's own document (`Message.Reach`) from the record that made
-- the post, so it is read with the message and pruned and erased with it.
--
-- WHETHER EACH OF THEM ANSWERED changes with LATER messages, so it is this
-- table: a row per (message, seat), written beside the post, and marked by the
-- applier when that seat posts in the message's thread (in a direct
-- conversation, anywhere after it). A later post finds the rows it answers on
-- `chat_receipts_open_idx`, which holds only the rows still waiting, so the
-- cost of a post is the handful of questions its author still owes in that
-- room, not the room's history.
--
-- # Derived from the records alone
--
-- Every column is copied from the post's record or the answering record (the
-- broker's instant, the room's minted sequence), so every node writes the same
-- rows and the identity claim covers the table. `message_at` is the post's own
-- instant, carried so the retention prune's range delete reaches these rows on
-- an index rather than through a join, as `chat_mentions` does.

CREATE TABLE chat_receipts (
    message_id  TEXT    NOT NULL,
    handle      TEXT    NOT NULL,
    channel_id  TEXT    NOT NULL,
    -- The thread an answer is looked for in: the message's own id for a room
    -- post, the thread's root for a reply.
    thread_key  TEXT    NOT NULL,
    -- The message's `channel_seq`: only a post after it can answer it.
    message_seq INTEGER NOT NULL,
    message_at  INTEGER NOT NULL,
    -- The message that answered, and the broker's instant for it; empty and
    -- NULL while the seat has not answered.
    answer_id   TEXT    NOT NULL DEFAULT '',
    answered_at INTEGER,
    PRIMARY KEY (message_id, handle)
);
CREATE INDEX chat_receipts_open_idx ON chat_receipts (channel_id, handle, thread_key) WHERE answer_id = ''; -- what a seat still owes in a room
CREATE INDEX chat_receipts_prune_idx ON chat_receipts (channel_id, message_at);                         -- the retention prune
