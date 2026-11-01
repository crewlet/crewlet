-- A change record says whom it reached.
--
-- Every record about a change carries the snapshot of whom it concerns
-- (code.ChangeRouting): who made it, and the handles each reason it routes
-- under reaches, as the deciding node's rows and chart held them. The applier
-- writes that answer down as ONE ROW PER (record, recipient) it reached — every
-- handle the routing's precedence keeps (code.Concerns) but the record's own
-- actor, who is told nothing they did — the way tracker_notifications is
-- written by the tracker's task and person records (0002). No two records
-- write one row: the key is the record and the recipient, and a record reaches
-- one handle once.
--
-- It is what a person's code inbox is read from (the inbox_idx below) and
-- what a change's "who did this wake" is answered from (the change_idx), and
-- what the dashboard's socket is told moved (internal/inbox's movements).
-- A SEAT is woken through the code log's feed, never from these rows; its row
-- is the record of the wake, which the turn-detail history answers beside.
--
-- # Two horizons, and none past its own
--
-- A PERSON's notice is kept the company's inbox.retention_days (a year by
-- default), its inbox's horizon. A SEAT's is kept store.EventHistory, thirty
-- days (ADR-0021): past it the turn a wake started is gone from the event log,
-- so the row would name a delivery nothing can show any more, and seats are
-- most of a change's audience, so a year of their rows would be most of this
-- table for nothing. recipient_kind says which a
-- row is, so each sweep deletes its own kind (the swept_idx). THE APPLIER
-- WRITES NO ROW PAST ITS HORIZON — a record whose broker instant is older
-- than its batch's instant less the recipient's horizon writes none for them
-- — so a replay of the whole log rebuilds no notice its sweep would delete.
--
-- Class: code_notifications is DIVERGENT. It is written only by an applied
-- record and travels inside a snapshot, but it is outside the identity claim:
-- whether a row is written depends on the batch's own instant and on the
-- horizon the epoch states, so two nodes applying one record at two moments,
-- or on two epochs, legitimately hold different rows — which is also why
-- each node sweeps its own (maintenance.NodeLocal), and why a repository's
-- purge charges a purged change's notices to its bounded parts, whose counts
-- must agree on every node, by the count the change keeps of them rather
-- than by these rows (0101).
--
-- # The columns
--
-- record_id is the record's operation id; recipient the handle it reached —
-- a seat's, or the human seat a person holds — and recipient_kind which;
-- repo_id, change_id and change_ref the change it is about, as the record's
-- routing names it (a push that merges several changes reaches each author
-- about the first change that names them); reason, addressed, fallback_only and
-- fallback_rank the precedence rule's answer (internal/routing); excerpt the
-- first 600 bytes of what the record says — a comment's body, a review's
-- summary, an open's description — and nothing for a record that says none;
-- created_at the record's broker instant, which both horizons compare
-- against; log_seq, log_stream and log_generation where the record sits on
-- the code log, the newest-first order of an inbox.
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
    PRIMARY KEY (record_id, recipient)
);

CREATE INDEX code_notifications_inbox_idx ON code_notifications (recipient, log_seq DESC);    -- a person's code inbox, newest first
CREATE INDEX code_notifications_change_idx ON code_notifications (change_id, log_seq);    -- whom a change's records reached, and a purge's removal of a purged change's notices
CREATE INDEX code_notifications_swept_idx ON code_notifications (recipient_kind, created_at);    -- each kind's sweep past its horizon, a range delete
