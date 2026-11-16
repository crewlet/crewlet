-- A comment record says whom it told, and a person keeps their marks over it.
--
-- Every comment record that opens a thread, replies in one, resolves or
-- reopens one carries the routing snapshot the API stamped on it
-- (artifactsdomain.CommentNotify): the agents it wakes and the PEOPLE it
-- tells. The artifacts applier writes the people's half down as ONE ROW PER
-- (record, person) it told, never the record's own author, the way Gitlet
-- writes code_notifications (0109). It is what a person's artifacts inbox is
-- read from (the inbox_idx below) and what the dashboard's socket is told moved
-- (internal/inbox's movements). An agent is woken through the artifacts log's
-- feed and has no row here.
--
-- # Kept the company's inbox horizon, and swept past it
--
-- A notice is kept the company's inbox.retention_days (a year by default).
-- THE APPLIER WRITES NO ROW PAST IT: a record whose broker instant is older
-- than its batch's instant less the horizon writes none, so a replay of the
-- whole log rebuilds no notice the sweep would delete. Each node sweeps its
-- own rows past it (the swept_idx).
--
-- Class: artifact_notifications is DIVERGENT, for code_notifications' reason:
-- written only by an applied record and travelling inside a snapshot, but
-- outside the identity claim, since whether a row is written depends on the
-- batch's own instant and the horizon its epoch states. An artifact's purge
-- deletes its rows; a remark's removal clears the excerpt of every notice its
-- record wrote, so the words a person removed are kept nowhere.
--
-- # The columns
--
-- record_id is the record's operation id and recipient the human seat it
-- told; artifact_id, thread_id and remark_id what it is about (remark_id ''
-- for a resolve or a reopen, which makes no remark); op what the record did
-- (comment_open, comment_reply, comment_resolve or comment_reopen); reason and
-- addressed the routing snapshot's answer (owner, assignee, mention or
-- participant, and whether it asks something of them); excerpt the first 600
-- bytes of the remark the record made, '' for none; actor and actor_kind who
-- wrote it; created_at the record's broker instant, which the horizon is
-- compared against; log_seq (packed), log_stream and log_generation where the
-- record sits on the artifacts log, the newest-first order of an inbox.
CREATE TABLE artifact_notifications (
    record_id      TEXT    NOT NULL,
    recipient      TEXT    NOT NULL,
    artifact_id    TEXT    NOT NULL,
    thread_id      TEXT    NOT NULL,
    remark_id      TEXT    NOT NULL,
    op             TEXT    NOT NULL,
    reason         TEXT    NOT NULL,
    addressed      INTEGER NOT NULL,
    excerpt        TEXT    NOT NULL,
    actor          TEXT    NOT NULL,
    actor_kind     TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    log_seq        INTEGER NOT NULL,
    log_stream     TEXT    NOT NULL,
    log_generation INTEGER NOT NULL,
    PRIMARY KEY (record_id, recipient)
);

CREATE INDEX artifact_notifications_inbox_idx ON artifact_notifications (recipient, log_seq DESC);   -- a person's artifacts inbox, newest first
CREATE INDEX artifact_notifications_remark_idx ON artifact_notifications (artifact_id, remark_id);   -- a purge's removal of an artifact's notices, and a removed remark's excerpts cleared
CREATE INDEX artifact_notifications_swept_idx ON artifact_notifications (created_at);   -- the sweep past the horizon, a range delete

-- artifact_person_marks: what a person DID about their artifacts notices, one
-- row a person, written by the `inbox_marks` records on the person's own
-- reader subject, each one gesture applied to the row its predecessor left
-- (internal/inbox's rules, as code_person_marks keeps Gitlet's, 0120). A mark
-- over the feed, never the feed: a person who never marked anything has no
-- row and still an inbox.
--
-- ON THE ARTIFACTS LOG, beside the notices it marks: a position is a
-- coordinate on ONE stream, so the seen-through position this row holds
-- counts in the artifacts log's sequence and no other.
--
-- handle is the human seat the person holds; generation the stream identity
-- the seen-through position was last moved on; seen_stream and seen_through
-- that position, PACKED; read, unread and snoozed the three lists of entries
-- the position does not already answer, each a JSON array of {record_id,
-- position[, until]} at most 256 long; primary_reasons the person's declared
-- split, a JSON array, empty for "I have not said"; position the record that
-- last wrote the row, packed.
--
-- Class: artifact_person_marks is REPRODUCIBLE: written only by the applier,
-- from the records on the person's own subject alone, each apply a function of
-- the record, its broker instant and the row before it. It names no artifact,
-- so no purge removes it: a mark naming a purged artifact's notice is an entry
-- nothing reads, pruned by the next gesture that moves the position past it.
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
