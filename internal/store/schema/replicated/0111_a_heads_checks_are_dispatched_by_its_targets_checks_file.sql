-- A head's checks are dispatched by its target's checks file.
--
-- A repository's home dispatches a change head's checks from its TARGET's
-- .crewlet/checks.yaml, read at the target's tip, and then records that it did
-- — a check_dispatched record on the repository's checks subject naming the
-- target, the tip it read, the blob it found (none, where the tip holds no
-- checks file) and the heads it dispatched under it (internal/code's
-- checksdispatch.go). These two tables keep what those records say, so the
-- home's due listing (checks_due) is a function of the rows alone: a live
-- change's head is due while no dispatch of it for its target names the
-- checks file the target's row holds, and a target is due a reading again
-- where its branch has moved past the tip its row was read at and the row is
-- older than the home's refresh.
--
-- The listing used to ask whether the head's commit held an attempt of any
-- check. A target with no checks file — every repository that has not adopted
-- checks — never gets one, so each of its heads stayed listed for ever, read
-- again on every pass of every home; and a check a target's file gained never
-- reached a head that held an attempt of the old file's.
--
-- code_check_targets is one row a repository and a branch its changes have
-- targeted: the checks file's blob at the tip the home last read it at
-- (defs_oid, '' for none), that tip (tip_oid) and the position of the record
-- that moved the branch there (tip_position), and the record that said so —
-- its broker instant (at), which the listing's refresh term compares with the
-- home's clock less its refresh, and its position.
--
-- code_check_dispatches is one row a head commit and a target branch: the
-- checks file the head was dispatched under (defs_oid), the change and
-- revision the home read it as, the target tip position it was read at, and
-- the record that said so.
--
-- ADDITIVE RECORDS, SO EACH ROW IS A JOIN: a row keeps the record that read
-- the later tip — the higher tip_position — then the later record (position),
-- whatever order two of them landed in.
--
-- NO SECONDARY INDEX: every read is by a row's whole key — the listing's
-- tests of a change's head and of its target, the join's and the decide's
-- reads — or a repository's key prefix, the purge's. A dispatch row is a few
-- dozen bytes a head, beside the revision row its head already is, which the
-- repository keeps as long; both tables go with the repository's purge.
--
-- Class: code_check_targets and code_check_dispatches are reproducible:
-- written only by the applier, from the check_dispatched records on a
-- repository's checks subject, and removed only by the repository's purge.
CREATE TABLE code_check_targets (
    repo_id      TEXT    NOT NULL,
    target_ref   TEXT    NOT NULL,
    tip_oid      TEXT    NOT NULL,
    tip_position INTEGER NOT NULL,
    defs_oid     TEXT    NOT NULL,
    at           INTEGER NOT NULL,
    position     INTEGER NOT NULL,
    PRIMARY KEY (repo_id, target_ref)
);

CREATE TABLE code_check_dispatches (
    repo_id      TEXT    NOT NULL,
    commit_oid   TEXT    NOT NULL,
    target_ref   TEXT    NOT NULL,
    defs_oid     TEXT    NOT NULL,
    change_id    TEXT    NOT NULL,
    revision     INTEGER NOT NULL,
    tip_position INTEGER NOT NULL,
    at           INTEGER NOT NULL,
    position     INTEGER NOT NULL,
    PRIMARY KEY (repo_id, commit_oid, target_ref)
);
