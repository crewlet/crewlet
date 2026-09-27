-- A chunk row names its SLOT, not its placement group.
--
-- # Why the group had to go
--
-- 0023 stored each chunk's placement group, `pg`, as the record carried it:
-- the address's first four bytes modulo 256, the map's fixed group count then.
-- The map now SPLITS its groups as the fleet grows (internal/objstore/
-- placement), so a group number written into a row is wrong the moment the
-- count doubles — and a pass reading "group 17's references" at the new count
-- would read the rows of a different run of addresses. What is fixed for ever
-- is the SLOT: the first two bytes of the address, big-endian, 0..65535. Every
-- group at every count is a contiguous run of slots, so a pass over one group
-- is one range of `tracker_file_chunks_slot_idx` whatever the map says.
--
-- `pg` is no help in computing it. Four bytes modulo 256 is the FOURTH byte,
-- and the slot is the first two, so no arithmetic on the old column reaches
-- the new one: the slot is read off the chunk's own address below.
--
-- # A copy of every row, never a drop-and-refill
--
-- Nothing refills this table. Its rows are written by the tracker's applier
-- from file records, and the checkpoint is already past every one of those —
-- so a table emptied here stays empty until each file is written again. And an
-- empty table is not a slow repair, it is data loss: the object store's
-- collector reads a chunk no row names as garbage and deletes it once it is
-- older than its grace (upkeep.PendingGrace, a day since it was written) —
-- which, for every file written more than a day ago, is the collector's next
-- pass. So every row is carried across with its slot computed from the chunk
-- it names.
--
-- # The expression reads LOWERCASE HEX, and that is enough
--
-- The slot is the first four hex digits of `chunk` read as a number — each
-- digit's position in '0123456789abcdef', less one, times its place value.
-- `instr` answers 0 for a character that is not there, so an uppercase digit
-- would yield a wrong (possibly negative) slot. No row carries one: the applier
-- that wrote every row refused a record naming a chunk that is not sixty-four
-- lowercase hex digits (objstore.Hash.Valid), and still does.
--
-- # The table is rebuilt rather than given a column with a default
--
-- `ALTER TABLE … ADD COLUMN slot INTEGER NOT NULL` needs a DEFAULT, and the
-- default would outlive this migration: an insert that forgot the column would
-- file its chunk at slot 0 — a real slot — instead of being refused. 0006 met
-- the same choice for `search_shard` and took NOT NULL with no default for the
-- same reason. So the old table is renamed aside, the new one is created in the
-- final shape, the rows are copied with their slots, and the old one is
-- dropped.
--
-- IT IS THE OLD TABLE THAT MOVES, not a new one created under a temporary name
-- and renamed into place: the estate gates (internal/store) read which tables a
-- file leaves behind from its CREATE and DROP statements, and this way round
-- the CREATE names the table the estate keeps. The old index is dropped by
-- name first — a RENAME carries it along, and DROP TABLE would take it anyway —
-- so the file says where `tracker_file_chunks_pg_idx` went.
--
-- 0023 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change. Its index comment names a reader
-- that no longer exists; the comment on the index below is the current one.
DROP INDEX tracker_file_chunks_pg_idx;

ALTER TABLE tracker_file_chunks RENAME TO tracker_file_chunks_by_group;

CREATE TABLE tracker_file_chunks (
    file_id TEXT    NOT NULL,
    seq     INTEGER NOT NULL,
    chunk   TEXT    NOT NULL,
    size    INTEGER NOT NULL,
    slot    INTEGER NOT NULL,
    PRIMARY KEY (file_id, seq)
);

INSERT INTO tracker_file_chunks (file_id, seq, chunk, size, slot)
SELECT file_id, seq, chunk, size,
       (instr('0123456789abcdef', substr(chunk, 1, 1)) - 1) * 4096
     + (instr('0123456789abcdef', substr(chunk, 2, 1)) - 1) * 256
     + (instr('0123456789abcdef', substr(chunk, 3, 1)) - 1) * 16
     + (instr('0123456789abcdef', substr(chunk, 4, 1)) - 1)
FROM tracker_file_chunks_by_group;

DROP TABLE tracker_file_chunks_by_group;

CREATE INDEX tracker_file_chunks_slot_idx ON tracker_file_chunks (slot, chunk);   -- one slot range's referenced chunks: upkeep's per-group read (objstore.ReferenceTable.ChunksIn)
