-- A person's code inbox keeps its marks.
--
-- A person's code inbox is read from the notices the applier writes per record
-- and recipient (code_notifications, 0081). What the person DID about them —
-- how far they have read on the code log, the notices they marked read out of
-- order or unread again, the ones they put off until an instant, and which
-- reasons are theirs to act on — is their own record, kept here: one row a
-- person, written by the `marks` records on the person's own subject
-- (person.<handle>), each one gesture applied to the row its predecessor left
-- (internal/inbox's rules, the tracker's person record's own). A mark over the
-- feed, never the feed: a notice the person never touched has no entry, and a
-- person who never marked anything has no row and still an inbox.
--
-- ON THE CODE LOG, beside the notices it marks, never on the tracker's person
-- record: a position is a coordinate on ONE stream, so the seen-through
-- position this row holds counts in the code log's sequence and no other, and
-- a person's tracker marks count in the tracker's.
--
-- # The columns
--
-- handle is the human seat the person holds; generation the stream identity
-- the seen-through position was last moved on (internal/inbox's third belt);
-- seen_stream and seen_through that position, PACKED — (generation << 40) |
-- seq, as every durable position is kept — so a reanchor's new generation
-- compares above every position before it; read, unread and snoozed the
-- three lists of entries the position does not already answer, each a JSON
-- array of {record_id, position[, until]} and at most 256 long (inbox.
-- MaxEntries); primary_reasons the person's declared split, a JSON array, or
-- empty for "I have not said", which reads as the shipped default; position
-- the record that last wrote the row, packed.
--
-- No index beside the key: every read of a row is one person's, by their
-- handle — a mark's decide and an inbox read alike.
--
-- Class: code_person_marks is REPRODUCIBLE: written only by the applier, from
-- the records on a person's own subject alone, each apply a function of the
-- record, its broker instant and the row before it, so a replay rebuilds it on
-- every node byte for byte. It names no repository, so no purge removes it: a
-- mark naming a purged change's notice is an entry nothing reads, pruned by the
-- next gesture that moves the position past it.
CREATE TABLE code_person_marks (
    handle          TEXT    NOT NULL PRIMARY KEY,
    generation      TEXT    NOT NULL DEFAULT '',
    seen_stream     TEXT    NOT NULL DEFAULT '',
    seen_through    INTEGER NOT NULL DEFAULT 0,
    read            TEXT    NOT NULL DEFAULT '[]',
    unread          TEXT    NOT NULL DEFAULT '[]',
    snoozed         TEXT    NOT NULL DEFAULT '[]',
    primary_reasons TEXT    NOT NULL DEFAULT '[]',
    position        INTEGER NOT NULL
);
