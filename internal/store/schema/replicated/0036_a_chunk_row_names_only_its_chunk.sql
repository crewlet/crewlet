-- A chunk row names its chunk and nothing about where the chunk lives.
--
-- 0032 gave every row a `slot` — the first two bytes of the chunk's address —
-- so a data node's repair could read one placement group's chunks as one
-- range of an index. The placement map, its groups and the repair are gone
-- (ADR-0026): the bytes live in ONE store the whole fleet shares, and the only
-- reader of this table is the collector asking which of a batch of chunks the
-- estate still names, which is a seek on the chunk itself. So the column and
-- its index go, and the chunk gets the index the collector reads.
--
-- 0031 AND 0032 ARE NOT EDITED: `schema_migrations` keys on the filename, so a
-- database that applied them would never see the change.
DROP INDEX tracker_file_chunks_slot_idx;

ALTER TABLE tracker_file_chunks RENAME TO tracker_file_chunks_by_slot;

CREATE TABLE tracker_file_chunks (
    file_id TEXT    NOT NULL,
    seq     INTEGER NOT NULL,
    chunk   TEXT    NOT NULL,
    size    INTEGER NOT NULL,
    PRIMARY KEY (file_id, seq)
);

INSERT INTO tracker_file_chunks (file_id, seq, chunk, size)
SELECT file_id, seq, chunk, size FROM tracker_file_chunks_by_slot;

DROP TABLE tracker_file_chunks_by_slot;

CREATE INDEX tracker_file_chunks_chunk_idx ON tracker_file_chunks (chunk);   -- which of a batch of chunks any file names: the collector (objstore.ReferenceTable.ChunksAmong)
