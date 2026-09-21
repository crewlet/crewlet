-- The chart domain's three GATE tables.
--
-- 0015 created the objects, the edges, the history and the import ledger —
-- everything a record's payload rebuilds. These three are the other half: what
-- the applier reads BEFORE it writes anything, and what stays true after the
-- record that wrote it has been trimmed away.
--
-- THEY ARE A SECOND MIGRATION RATHER THAN AN EDIT TO 0015, which is the rule
-- `schema_migrations` forces and not a preference: it keys on the FILENAME, so
-- a table added by editing a file that has already run never runs on any
-- database that applied it — and that database then serves a build whose
-- applier reads a table it does not have. A domain landing across two
-- migrations reads slightly worse than one; a silent divergence between two
-- copies of the same estate reads fine right up until somebody trusts one.

-- ---------------------------------------------------------------------------
-- chart_evictions — a node's removal from THIS log, and its readmission.
-- ---------------------------------------------------------------------------
--
-- The gate is "records this node wrote ABOVE this position", and the position
-- is the log's own — so every node reaches the same verdict about every record
-- with no clock, no coordination read and no agreement beyond the order they
-- all already have. That is what makes it the fence that still holds when
-- coordination cannot be reached at all, which is the only state in which an
-- eviction is permitted at all.
CREATE TABLE chart_evictions (
    node_id             TEXT    NOT NULL PRIMARY KEY,
    at                  INTEGER NOT NULL,
    by                  TEXT    NOT NULL DEFAULT '',
    from_position       INTEGER NOT NULL,
    -- NULL until a readmission, which is an INVERSE COMMIT rather than a
    -- delete: an eviction's whole history survives a replay, and a node that
    -- was evicted, readmitted and evicted again reads correctly rather than as
    -- one long absence.
    readmitted_position INTEGER,
    version             INTEGER NOT NULL
);

-- ---------------------------------------------------------------------------
-- chart_log_generations — every reanchor's audit row.
-- ---------------------------------------------------------------------------
--
-- A reanchor is a committed record and therefore reproducible from the log;
-- this is what a recovery replay writes and what the operator surface reads.
CREATE TABLE chart_log_generations (
    generation            INTEGER NOT NULL PRIMARY KEY,
    at                    INTEGER NOT NULL,
    by                    TEXT    NOT NULL DEFAULT '',
    new_stream_created_at INTEGER NOT NULL DEFAULT 0,
    prev_last_seq_seen    INTEGER NOT NULL DEFAULT 0,
    record_id             TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- chart_removed — what the chart no longer names.
-- ---------------------------------------------------------------------------
--
-- THE ONE OPERATION HERE WITH NO INVERSE, which is why it leaves a row behind
-- rather than only deleting one. Every other record is a full post-state under
-- a monotone guard, so a node that applied a stale one is repaired by the next
-- record on that object; nothing ever names a removed object again. Without
-- this row a redelivery of any earlier record on a removed seat would write
-- the object back, on one node, for ever — and the fleet's copies would differ
-- in a table that claims identity.
--
-- IT ALSO OUTLIVES THE RECORD. A removal below the trim floor has no record
-- left on the log to prove it happened, and this row is what still says so:
-- it is what a replay from a snapshot taken after the removal reproduces, and
-- what a write fence reads before it publishes at an expectation of zero.
--
-- THE REASON IS A COLUMN because a removal is the one apply whose consequences
-- outlive its object: a seat leaving the chart releases a mailbox and retires a
-- coding run, and the person asking why next month has only this row to read.
CREATE TABLE chart_removed (
    -- The kind and the id together, because a unit key and a seat handle are
    -- different namespaces: `chart.NormalizeKey` folds both and neither
    -- reserves a prefix, so a company may legitimately hold a unit and a seat
    -- that answer to one spelling.
    object_kind TEXT    NOT NULL,
    object_id   TEXT    NOT NULL,
    at          INTEGER NOT NULL,
    -- The record that removed it, by its OPERATION ID rather than its
    -- position: the apply that writes this row is the one that must be able to
    -- run twice, and a position changes under a republish where an op id does
    -- not.
    record_id   TEXT    NOT NULL DEFAULT '',
    actor       TEXT    NOT NULL DEFAULT '',
    actor_kind  TEXT    NOT NULL DEFAULT '',
    reason      TEXT    NOT NULL DEFAULT '',
    version     INTEGER NOT NULL,
    PRIMARY KEY (object_kind, object_id)
);
-- The gate's own read is by (kind, id) and is served by the primary key. This
-- index is for the OTHER reader: the operator surface asking what left the
-- chart recently, and the sweep that has a horizon to range over.
CREATE INDEX chart_removed_at_idx ON chart_removed (at);
