-- A change record reaches a handle once A CHANGE, not once a record.
--
-- 0081 keyed code_notifications on (record_id, recipient): a record reached
-- one handle once. But one record can carry news of several changes — the
-- committer folds up to 64 pushes from unrelated clients into one push
-- record, each opening or merging changes of its own, and a queue's landing
-- or ejection carries one routing for every entry it lands or puts out — so a
-- seat mentioned by two of a batch's changes, or the author of two changes
-- one landing merged, heard about the first and nothing of the second, and
-- the busier the repository the more of its news was lost. A notice is now
-- one (record, change, recipient), what code.NoticeID names: the identity a
-- person's mark names and a seat's wake is derived from.
--
-- 0081 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change. The table is rebuilt with the
-- new key and its three indexes, every row carried over — each was one
-- record's notice about the one change it names, so no two collide.
--
-- Class: code_notifications stays DIVERGENT (0081): written only by an
-- applied record, travelling inside a snapshot, outside the identity claim.
DROP INDEX code_notifications_inbox_idx;
DROP INDEX code_notifications_change_idx;
DROP INDEX code_notifications_swept_idx;

ALTER TABLE code_notifications RENAME TO code_notifications_by_record;

CREATE TABLE code_notifications (
    record_id      TEXT    NOT NULL,
    recipient      TEXT    NOT NULL,
    recipient_kind TEXT    NOT NULL,
    repo_id        TEXT    NOT NULL,
    change_id      TEXT    NOT NULL,
    change_ref     TEXT    NOT NULL,
    reason         TEXT    NOT NULL,
    addressed      INTEGER NOT NULL,
    fallback_only  INTEGER NOT NULL,
    fallback_rank  INTEGER NOT NULL,
    excerpt        TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    log_seq        INTEGER NOT NULL,
    log_stream     TEXT    NOT NULL,
    log_generation INTEGER NOT NULL,
    PRIMARY KEY (record_id, change_id, recipient)
);

INSERT INTO code_notifications (record_id, recipient, recipient_kind, repo_id, change_id, change_ref, reason,
    addressed, fallback_only, fallback_rank, excerpt, created_at, log_seq, log_stream, log_generation)
SELECT record_id, recipient, recipient_kind, repo_id, change_id, change_ref, reason, addressed, fallback_only,
    fallback_rank, excerpt, created_at, log_seq, log_stream, log_generation
FROM code_notifications_by_record;

DROP TABLE code_notifications_by_record;

CREATE INDEX code_notifications_inbox_idx ON code_notifications (recipient, log_seq DESC);    -- a person's code inbox, newest first
CREATE INDEX code_notifications_change_idx ON code_notifications (change_id, log_seq);    -- whom a change's records reached, and a purge's removal of a purged change's notices
CREATE INDEX code_notifications_swept_idx ON code_notifications (recipient_kind, created_at);    -- each kind's sweep past its horizon, a range delete
