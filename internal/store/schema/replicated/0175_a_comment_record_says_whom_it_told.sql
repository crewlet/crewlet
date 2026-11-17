-- A comment record says whom it told: one row per (record, person) told, never its author, as code_notifications (0109); agents are woken by the log's feed instead.
-- Class: DIVERGENT, as code_notifications: no row is written past the company's inbox horizon (by the batch's instant) and each node sweeps its own past it.
-- A purge deletes an artifact's rows, and a removed remark clears the excerpt of every notice its record wrote.
CREATE TABLE artifact_notifications (
    record_id      TEXT    NOT NULL,
    recipient      TEXT    NOT NULL,
    artifact_id    TEXT    NOT NULL,
    thread_id      TEXT    NOT NULL,
    remark_id      TEXT    NOT NULL,
    op             TEXT    NOT NULL,   -- comment_open, comment_reply, comment_resolve or comment_reopen
    reason         TEXT    NOT NULL,   -- owner, assignee, mention or participant
    addressed      INTEGER NOT NULL,   -- 1 when the remark asks something of the recipient
    excerpt        TEXT    NOT NULL,   -- the first 600 bytes of the remark, '' for none
    actor          TEXT    NOT NULL,
    actor_kind     TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,   -- the record's broker instant, compared against the horizon
    log_seq        INTEGER NOT NULL,   -- packed; the inbox's newest-first order
    log_stream     TEXT    NOT NULL,
    log_generation INTEGER NOT NULL,
    PRIMARY KEY (record_id, recipient)
);

CREATE INDEX artifact_notifications_inbox_idx ON artifact_notifications (recipient, log_seq DESC);   -- a person's artifacts inbox, newest first
CREATE INDEX artifact_notifications_remark_idx ON artifact_notifications (artifact_id, remark_id);   -- a purge's removal of an artifact's notices, and a removed remark's excerpts cleared
CREATE INDEX artifact_notifications_swept_idx ON artifact_notifications (created_at);   -- the sweep past the horizon, a range delete

-- artifact_person_marks: what a person did about their artifacts notices, one row a person, written by the `inbox_marks` records on their reader subject (internal/inbox, as code_person_marks, 0120).
-- Its seen-through position counts in the artifacts log's sequence; read, unread and snoozed are JSON arrays (at most 256) of entries that position does not answer.
-- Class: REPRODUCIBLE, written only by the applier; it names no artifact, so no purge removes it.
CREATE TABLE artifact_person_marks (
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
