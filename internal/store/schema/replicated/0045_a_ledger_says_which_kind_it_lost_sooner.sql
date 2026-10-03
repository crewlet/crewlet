-- A ledger that sweeps one subject KIND sooner than the rest records that
-- kind's loss apart from the table's.
--
-- # What was colliding
--
-- 0017 gave every operation ledger ONE watermark: the instant before which it
-- may have lost rows. The publisher reads it before it decides anything, and
-- an operation minted before it is vouched for only by its own row — absence
-- there may be a row the sweep took, so the write is answered `unknown`
-- without publishing rather than decided a second time.
--
-- 0038 gave the identity estate's ledger a SECOND horizon: a session subject's
-- rows go after an hour, everything else's after the framework's month. Moving
-- 0017's one watermark on that hourly sweep would say the WHOLE ledger lost
-- everything older than an hour — and every identity operation minted before
-- that (an invitation redeemed under an id derived from its issue, a create
-- retried under its operation key, a token mint's retry) would be answered
-- `unknown` without ever being published. Not moving it would let a session
-- operation whose row the hour took be decided again, which is the double
-- apply the watermark exists to prevent.
--
-- # The shape
--
-- One row per (ledger, subject kind) that a kind-specific sweep has lost rows
-- of, written in the very transaction that deletes them, and only ever moved
-- forward. What the publisher weighs for an operation is the later of the
-- table's watermark and its subject kind's — so a session operation is held to
-- the hour and every other kind to the month.
--
-- IT TRAVELS AS 0017'S DOES: a snapshot carries the replicated file, so an
-- adopter inherits how far back each kind of the donor's ledger may have lost
-- rows along with the rows themselves. An absent row means that kind lost
-- nothing beyond the table-wide watermark.
CREATE TABLE statelog_ops_lost_kind (
    ops_table    TEXT    NOT NULL,
    subject_kind TEXT    NOT NULL,
    lost_before  INTEGER NOT NULL,
    PRIMARY KEY (ops_table, subject_kind)
);
