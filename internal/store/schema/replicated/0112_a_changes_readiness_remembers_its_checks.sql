-- A change's readiness remembers its checks.
--
-- A change's stored readiness (code_mergeability.ready and waits_person, 0101)
-- is judged by a `mergeability` record's decide over its own snapshot and
-- refreshed by its home whenever what it reads moved past the versions it was
-- judged at — its row and its conversation's readiness inputs. The checks of
-- its head are inputs too (M5-D26): an attempt queued, a requeue, an ending —
-- each a record on the repository's CHECKS subject, which moves neither
-- version. So the row now also remembers the position of the latest record
-- that queued or ended an attempt of the head's checks — the attempts at the
-- head FOR THE CHANGE'S TARGET, the ones its merge gate judges (0108) — as the
-- decide read it (checks_position), and the home's `changes_due` listing names
-- a change whose head's attempts moved past it: its refresh is recorded above
-- the stored row rather than refused superseded, and since the merge gate
-- judges checks it is where `ready` flips and ready_to_merge is announced.
--
-- CODE_MERGEABILITY IS CREATED AGAIN, EMPTY, RATHER THAN ALTERED, for 0101's
-- reason: the due listing reads checks_position of every live change on every
-- home pass, and a column ALTER TABLE adds goes after the conflicted paths,
-- the row's wide column, where reading it would read each row's overflow
-- chain. No release carries 0101: the code domain is on no build of main and
-- in no image or tag (the domain's stated condition, code.RecordVersion), so
-- no store holds a row of it outside a development branch, and a change whose
-- row is gone is due its merge again at its home's next pass — a function of
-- two commits, recorded again. 0101 IS NOT EDITED: `schema_migrations` keys on
-- the filename. NO DEFAULT on checks_position: a writer states it, 0 for a
-- head no attempt of which has been queued.
--
-- Class: code_mergeability stays reproducible: written only by the applier,
-- from the records on a change's own subject, and removed only by its
-- repository's purge, a change's mergeability before its row.
DROP TABLE code_mergeability;

CREATE TABLE code_mergeability (
    change_id         TEXT    NOT NULL PRIMARY KEY,
    revision          INTEGER NOT NULL,
    target_ref        TEXT    NOT NULL,
    target_oid        TEXT    NOT NULL,
    target_position   INTEGER NOT NULL,
    state             TEXT    NOT NULL,
    conflicts_count   INTEGER NOT NULL DEFAULT 0,
    conflicts_omitted INTEGER NOT NULL DEFAULT 0,
    node              TEXT    NOT NULL,
    bytes             INTEGER NOT NULL DEFAULT 0,
    position          INTEGER NOT NULL,
    at                INTEGER NOT NULL,
    change_version    INTEGER NOT NULL,
    readiness_version INTEGER NOT NULL,
    checks_position   INTEGER NOT NULL,
    ready             INTEGER NOT NULL,
    waits_person      INTEGER NOT NULL,
    conflicts         TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX code_mergeability_waits_idx ON code_mergeability (waits_person, change_id) WHERE waits_person = 1;    -- the changes waiting on a person, newest first: needs=person
