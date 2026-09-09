-- The state-log framework's own durable state, in the replicated estate
-- because contract 2 puts it in the applier's transaction and a transaction is
-- one file.
--
-- THE FRAMEWORK'S TABLES AND NOTHING ELSE. A domain's own tables — its rows,
-- its operation ledger, its deferred records and their scope index — belong to
-- the domain's own migration, so a second domain adds a file rather than
-- editing this one. This file is never edited after it ships: schema_migrations
-- keys on the FILENAME, so a change here silently never runs on any database
-- that already applied it, and every one of those keeps the old shape while the
-- code assumes the new one.
--
-- # The shape a domain's own two tables must have
--
-- Documented here rather than in a comment on a domain, because the framework
-- generates the statements that write them and a static test asserts every
-- registered domain against this:
--
--   <domain>_ops(
--       op_id      TEXT    NOT NULL PRIMARY KEY,
--       subject    TEXT    NOT NULL,   -- the full wire subject
--       position   INTEGER NOT NULL,   -- the COMPOSED (generation << 40) | seq
--       applied_at INTEGER NOT NULL)   -- for the sweep, which is a range
--                                      --   delete and needs an index on it
--   CREATE INDEX <domain>_ops_swept_idx ON <domain>_ops (applied_at);
--
--   <domain>_log_deferred(
--       position     INTEGER NOT NULL, -- the composed position, and the key on
--                                      --   a strict domain
--       subject      TEXT    NOT NULL, -- the key on a COMPACTED domain, where a
--                                      --   newer record REPLACES an older one
--                                      --   because the stream itself keeps one
--                                      --   message per subject
--       subject_kind TEXT    NOT NULL,
--       subject_id   TEXT    NOT NULL,
--       version      INTEGER NOT NULL, -- the record version this build could
--                                      --   not decode, which is the number an
--                                      --   operator needs to pick a build
--       payload      BLOB    NOT NULL, -- the record, byte for byte, because
--                                      --   LOSSLESS MEANS THE BYTES
--       stored_at    INTEGER NOT NULL,
--       PRIMARY KEY (position))
--   CREATE INDEX <domain>_log_deferred_subject_idx
--       ON <domain>_log_deferred (subject_id, subject_kind);
--
-- The scope child keys on whatever its parent keys on and is written in the
-- SAME transaction as it: a deferral recorded without its scope is a record
-- that makes rows stale with nothing able to see that it does.

-- statelog_cursor is how far each domain's applier has committed on this node.
--
-- ONE ROW PER STREAM. The position is stored as its two parts rather than as
-- the composed integer, because this is the one place both halves are read
-- back and reconstructed — everywhere else the composed form is what a column
-- compares with.
--
-- stream_created_at is the DETECTOR and the generation is the RESPONSE. A
-- recreated stream starts its sequences again, so every stored sequence is
-- suddenly a number in a space it does not belong to; the broker's own
-- creation instant is what NOTICES that, and the generation is what makes the
-- old numbers comparable and safely stale rather than plausible.
--
-- It TRAVELS inside a snapshot — it is what a recipient reads the artefact's
-- position from — and it is the value the identity claim is conditioned on, so
-- comparing it as part of that claim would be circular.
CREATE TABLE statelog_cursor (
    stream            TEXT    NOT NULL PRIMARY KEY,
    generation        INTEGER NOT NULL,
    seq               INTEGER NOT NULL,
    stream_created_at INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

-- statelog_anchor is what the broker arbitrates a write against: the position
-- of the LAST record this node consumed on a subject, whatever that record
-- then did — applied, dropped by a gate, or retained because this build could
-- not decode it.
--
-- # Why it is not the object's version
--
-- An expectation is a claim about the LOG — what is the last message on this
-- subject — and a version is a claim about the DOMAIN's accepted state. They
-- are equal only while every accepted record produces rows, and an apply GATE
-- is by definition the rule that makes them differ.
--
-- Measured against the shape this replaces: an evicted node's append is
-- accepted by the broker at 102 and dropped by every applier, so the object's
-- version stays 100 on every node in the fleet while the broker's last message
-- on that subject is 102. A healthy writer forms 100, is refused, re-reads
-- 100, and burns its whole round budget — for at least the trim's age floor,
-- and unbounded above while any retention term blocks. Every arbitrated
-- subject in a domain is wedgeable the same way, including the one that mints
-- task numbers, which stops the project accepting new work at all.
--
-- Keyed on the WIRE SUBJECT STRING deliberately: the invariant is then
-- directly checkable against the broker's own last-message answer with no
-- kind-to-subject mapping in between that could itself be the bug, and the
-- publisher already holds the string it is about to publish to.
CREATE TABLE statelog_anchor (
    stream  TEXT    NOT NULL,
    subject TEXT    NOT NULL,
    anchor  INTEGER NOT NULL,
    PRIMARY KEY (stream, subject)
);

-- The write path reads this table by exact key and needs no index for it. The
-- SWEEP does: it is a range delete over anchors below the trim floor, which
-- without this scans every subject in the company.
CREATE INDEX statelog_anchor_swept_idx ON statelog_anchor (stream, anchor);
