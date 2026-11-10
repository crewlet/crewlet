-- A node's standing alarms outlive its restart.
--
-- Each row is one alarm instance this node's tracker has up: its kind, the log
-- or configured entry it stands on (empty for a node-wide kind), the instant it
-- was raised and the detail its lines carry. The tracker's first evaluation
-- after a restart reads them back, so an instance still up is no transition, one
-- that cleared while the node was down is one clear with its original instant,
-- and only a new one fires. Without it every restart re-raised every standing
-- alarm and woke every health seat about it again.
--
-- WHO HAS TO AGREE ON IT: this node alone. An alarm instance is this node's
-- tracker's observation of its own evaluations; a peer evaluates its own.
CREATE TABLE alarm_standing (
    kind    TEXT    NOT NULL,
    domain  TEXT    NOT NULL DEFAULT '',
    entry   TEXT    NOT NULL DEFAULT '',
    since   INTEGER NOT NULL,
    detail  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (kind, domain, entry)
);
