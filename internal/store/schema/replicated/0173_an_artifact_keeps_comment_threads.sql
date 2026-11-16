-- An artifact keeps comment threads (ADR-0153).
--
-- A thread is its own subject on the artifacts log (`comment`, id
-- `<artifact>.<thread>`), written by the artifacts domain's one applier
-- (internal/artifactsdomain) and nothing else. A thread keeps the anchor it
-- was made with and the version it was made on, and nothing else about where
-- it is: its place on any version is found at read, from that version's files,
-- by internal/artifacts/commentanchor. A publish therefore writes no row here.
--
-- WHO HAS TO AGREE ON IT: every data node, identically. Both tables are in the
-- domain's reproducible set (artifactsdomain.ReproducibleTables), so a replay
-- from zero rebuilds them byte for byte and an identity claim compares them
-- across the fleet. The rules 0043 states for every replicated table hold
-- here: no foreign key (an artifact's purge deletes its threads and remarks in
-- the applier's own statement list), no UNIQUE outside a primary key, no
-- COLLATE, attribution is a seat handle beside its kind, and every instant is
-- the broker's. A comment record writes no history row: each row carries the
-- packed position of the last record that wrote it, which is what the writer
-- reads its own verdict back from.

-- artifact_comment_threads: one row per thread.
CREATE TABLE artifact_comment_threads (
    artifact_id      TEXT    NOT NULL,
    -- A name-based uuid over the opening operation, so a retry names the
    -- thread its first attempt opened.
    id               TEXT    NOT NULL,
    -- The version the thread was made on.
    version          INTEGER NOT NULL,
    -- The anchor as made, JSON (commentanchor.Anchor): its kind and what it
    -- is found again by on another version.
    anchor           TEXT    NOT NULL,
    -- The text the commenter selected, '' for none.
    quote            TEXT    NOT NULL DEFAULT '',
    -- A picture of the region the thread opened on (PNG, JPEG or WebP, at
    -- most artifacts.MaxCommentCropBytes), NULL for none, and its type.
    crop             BLOB,
    crop_type        TEXT    NOT NULL DEFAULT '',
    -- open or resolved.
    status           TEXT    NOT NULL,
    -- The last resolve: who, when, and the version that addressed the
    -- thread. Cleared by a reopen.
    resolved_by      TEXT    NOT NULL DEFAULT '',
    resolved_by_kind TEXT    NOT NULL DEFAULT '',
    resolved_at      INTEGER NOT NULL DEFAULT 0,
    resolved_version INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT    NOT NULL,
    created_by_kind  TEXT    NOT NULL,
    created_at       INTEGER NOT NULL,
    -- The last record on the thread, any of its remarks included.
    updated_at       INTEGER NOT NULL,
    -- The packed position of the last record that wrote this row.
    position         INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, id)
);
CREATE INDEX artifact_comment_threads_created_idx ON artifact_comment_threads (artifact_id, created_at, id);   -- one artifact's threads, oldest first

-- artifact_comments: one row per remark. A removed remark keeps its row, its
-- place and its author, with its words cleared.
CREATE TABLE artifact_comments (
    artifact_id TEXT    NOT NULL,
    thread_id   TEXT    NOT NULL,
    -- A name-based uuid over the operation that wrote the remark.
    id          TEXT    NOT NULL,
    author      TEXT    NOT NULL,
    author_kind TEXT    NOT NULL,
    -- Markdown, stored verbatim and never rendered by the engine.
    body        TEXT    NOT NULL,
    -- The seat handles the remark names, a sorted JSON array.
    mentions    TEXT    NOT NULL DEFAULT '[]',
    -- The remark this one answers, '' for the thread itself.
    reply_to    TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    -- The last edit, 0 for a remark never edited.
    edited_at   INTEGER NOT NULL DEFAULT 0,
    -- 1 once removed.
    removed     INTEGER NOT NULL DEFAULT 0,
    -- The packed position of the last record that wrote this row.
    position    INTEGER NOT NULL,
    PRIMARY KEY (artifact_id, thread_id, id)
);
CREATE INDEX artifact_comments_created_idx ON artifact_comments (artifact_id, thread_id, created_at, id);   -- one thread's remarks, oldest first
