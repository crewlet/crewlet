-- usage_held — a person's day this node's usage publisher derived and is
-- HOLDING: not yet published, because a node applying the usage log runs a
-- build that cannot read the record's version (internal/usage's publisher,
-- "A person's day waits for every reader").
--
-- WHO HAS TO AGREE ON IT: this node alone. Each node publishes only its own
-- days, derived from its own event log, so what it holds back is a fact about
-- its own pending publishes; a peer's copy would be a hold on records the peer
-- never derived. The answer every node agrees on — the published day — is the
-- replicated usage domain's, and a row here is what has not reached it yet.
--
-- WHY A TABLE AND NOT MEMORY: the publisher derives only today and yesterday,
-- so a day older than that is never derived again by any process. Held in
-- memory alone, a node restarted during a rolling upgrade lost every held day
-- older than yesterday for good: the spend stayed on the counter and the live
-- window and never reached the named windows, and nothing said so.
--
-- A ROW PER HELD OBJECT, replaced whenever the publisher re-derives it, and
-- deleted the moment it is published or falls past the usage history's
-- horizon — so the table is empty outside an upgrade and needs no sweep.
-- `record` is the encoded usage record, exactly as it will be published.
CREATE TABLE usage_held (
    day     TEXT    NOT NULL,
    subject TEXT    NOT NULL,
    record  BLOB    NOT NULL,
    held_at INTEGER NOT NULL,
    PRIMARY KEY (day, subject)
);
