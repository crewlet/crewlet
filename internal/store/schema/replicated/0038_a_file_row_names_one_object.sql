-- A file row names the one object its bytes were uploaded into.
--
-- A file's content used to be cut into content-addressed chunks, and
-- `tracker_file_chunks` held one row per chunk of a live file — the table the
-- collector read references from, with `tracker_files.chunks` counting them.
-- Content addressing is what let two files share a stored chunk, and a shared
-- chunk is one a writer can be re-using at the moment the collector deletes
-- it, which took a lock in the coordination store around every re-put and
-- every deletion. Each upload is now ONE object under a key minted for that
-- upload alone (ADR-0026, ADR-0027), which no other write ever names, so the
-- row names it directly: `object`, the key, beside the `hash` and `size` the
-- row always carried and every read is now checked against.
--
-- NULL is a row naming no object: a removed file, and a file an earlier build
-- kept in chunks — whose record the applier still applies, on every node
-- alike, as a live file with no content this build can read. That is exactly
-- what replaying the log from nothing writes, so a node migrated here and a
-- node that replays agree row for row. Every statement the object store builds
-- leaves NULL out by name.
--
-- The chunk rows go with their table: no row names a chunk any more, and the
-- chunks an earlier build stored are retired from the store once every node
-- has stopped writing them. Their bytes are not carried across.
--
-- No UNIQUE on `object`, for 0031's reason: a constraint violation inside an
-- apply would abort the same transaction on every node and stall the log.
--
-- 0031, 0032 AND 0036 ARE NOT EDITED: `schema_migrations` keys on the
-- filename, so a database that applied them would never see the change.
DROP INDEX tracker_file_chunks_chunk_idx;

DROP TABLE tracker_file_chunks;

ALTER TABLE tracker_files DROP COLUMN chunks;

ALTER TABLE tracker_files ADD COLUMN object TEXT;

CREATE INDEX tracker_files_object_idx ON tracker_files (object);   -- which of a batch of objects any file names, and every named object in key order: the collector, its audit and the backup (objstore.ReferenceTable.ObjectsAmong, ReferencesAfter)
