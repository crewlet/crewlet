-- A reader finds their own chat: the threads they are in, the moments they
-- were named, what they said, and the links a room shared.
--
-- # What was missing
--
-- Every read of the chat domain was about a room: its transcript, one thread,
-- its membership. The reader's own views ask across rooms instead, each for
-- one seat: Activity (where was I named, what was said in the threads I follow,
-- who reacted to me), Threads (the threads I follow, newest reply first), Sent
-- (what I said, newest first) and a room's Shared links. Each of those is a
-- range over one handle, or over the few messages of a room that carry a link,
-- and without an index it is a scan of the whole corpus, the highest-volume
-- table the engine has (0055's census: 20,000 messages a day).
--
-- # Why these six, and nothing else
--
-- `chat_thread_participants_handle_idx` and `chat_follows_handle_idx`: the
-- threads one seat has spoken in or been named in. 0055 left both tables keyed
-- by the thread alone, because then nothing asked which threads one seat
-- follows; now Threads and Activity do, and each drives from these and then
-- seeks each thread's replies on `chat_messages_thread_idx`.
--
-- `chat_mentions_named_idx`: one person's mention feed in WHEN-NAMED order.
-- Activity merges mentions with replies and reactions, which have no log
-- position of their creation to order by (a message's `version` moves with an
-- edit), so the three are ordered by the broker's instant, and a mention by
-- the instant it was named. `chat_mentions_handle_idx` (by position) still
-- serves the mention feed itself.
--
-- `chat_messages_sent_idx`: what one seat said, newest first, by the instant it
-- was said. `chat_messages_author_idx` orders by `version`, which an edit
-- moves, so a list built on it would lift an old message above newer ones the
-- moment it was corrected.
--
-- `chat_messages_collective_idx`: the messages that named a whole room
-- (`@channel`, 0071's `collective`), which name nobody in `chat_mentions`.
-- Activity lists them as mentions for every member of the room, reached from
-- the reader's memberships, so the range is per room and PARTIAL: few messages
-- name a whole room.
--
-- `chat_messages_links_idx`: a room's messages that carry a link. PARTIAL,
-- because few messages carry one: the index holds only those rows, so a room
-- of fifty thousand messages and forty links pages its links in forty rows
-- rather than walking the transcript.
--
-- # Derived from columns every node already writes identically
--
-- Indexes only, over replicated columns the applier writes from the same
-- records on every node, so no row changes and the identity claim is
-- untouched.

CREATE INDEX chat_thread_participants_handle_idx ON chat_thread_participants (handle, channel_id, thread_root); -- the threads one seat spoke in
CREATE INDEX chat_follows_handle_idx ON chat_follows (handle, channel_id, thread_root);                          -- the threads one seat was named in
CREATE INDEX chat_mentions_named_idx ON chat_mentions (handle, named_at DESC, message_id);                       -- one person's mentions, newest named first
CREATE INDEX chat_messages_sent_idx ON chat_messages (author_handle, created_at DESC, id);                       -- what one seat said, newest first
CREATE INDEX chat_messages_links_idx ON chat_messages (channel_id, channel_seq DESC) WHERE links <> '';           -- a room's shared links
CREATE INDEX chat_messages_collective_idx ON chat_messages (channel_id, created_at DESC) WHERE collective = 1; -- the messages that named a whole room
