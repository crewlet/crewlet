-- A project keeps files.
--
-- # What the two tables are
--
-- `tracker_files` is one row per file ADDRESS — a project and a path — keyed on
-- the file's subject id (`<PROJECT>.<token of the path>`), which is what two
-- writers of one path contend on. It holds what a listing shows and what a
-- reader needs to open the file; the bytes are NOT here. They are in the object
-- store, placed on a few data nodes rather than held by every one
-- (internal/objstore, ADR-0019).
--
-- `tracker_file_chunks` is the manifest exploded: one row per chunk of a live
-- file, in order. It is the table the object store's passes read a placement
-- group's references from, which is why it carries `pg` and why its one
-- secondary index leads with it.
--
-- # A removal keeps the row
--
-- A removed file keeps its `tracker_files` row, stamped, and loses its chunk
-- rows. The row is kept because every upsert here is guarded by the record's
-- version and SKIPS an older one — a deleted row would have nothing to compare
-- a redelivered put against, and the file would come back. The chunk rows go,
-- because a chunk nothing names is one the collector may delete.
--
-- # No UNIQUE on (project_key, path)
--
-- The subject id already IS (project, path), and a constraint violation inside
-- an apply would abort the same transaction on every node and stall the log —
-- see TestNoUniqueOutsideAPrimaryKey.
CREATE TABLE tracker_files (
    id           TEXT    NOT NULL PRIMARY KEY,
    project_key  TEXT    NOT NULL,
    path         TEXT    NOT NULL,
    content_type TEXT    NOT NULL DEFAULT '',
    hash         TEXT    NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    chunks       INTEGER NOT NULL DEFAULT 0,
    created_by   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_by   TEXT    NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    removed_by   TEXT    NOT NULL DEFAULT '',
    removed_at   INTEGER,
    version      INTEGER NOT NULL,
    document     BLOB    NOT NULL
);
CREATE INDEX tracker_files_project_idx ON tracker_files (project_key, path);   -- a project's files in path order: Reader.Files

CREATE TABLE tracker_file_chunks (
    file_id TEXT    NOT NULL,
    seq     INTEGER NOT NULL,
    chunk   TEXT    NOT NULL,
    size    INTEGER NOT NULL,
    pg      INTEGER NOT NULL,
    PRIMARY KEY (file_id, seq)
);
CREATE INDEX tracker_file_chunks_pg_idx ON tracker_file_chunks (pg, chunk);   -- one placement group's referenced chunks: FileChunkRefs.Referenced
