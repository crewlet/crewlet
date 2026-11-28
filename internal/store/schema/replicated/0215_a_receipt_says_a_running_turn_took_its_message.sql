-- A receipt says a seat's running turn took its message (ADR-0168's amendment
-- "a person's message is never dropped unseen").
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the chat
-- log by its one applier, as 0075 states for the rest of chat_receipts.
--
-- `taken` is what the seat's running turn did with the message instead of a
-- turn of its own: `joined` (taken into the running work) or `absorbed` (read
-- there, asking nothing), '' while it did neither; `taken_at` the broker's
-- instant of the record that said so. An answer is still marked over it.
ALTER TABLE chat_receipts ADD COLUMN taken TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_receipts ADD COLUMN taken_at INTEGER NOT NULL DEFAULT 0;
