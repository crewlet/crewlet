-- A change's readiness remembers its chart.
--
-- A change's stored readiness (code_mergeability.ready and waits_person, 0078)
-- is judged by a `mergeability` record's decide over its own snapshot AND ITS
-- NODE'S CHART: who may approve the change — a seat's tier and its place
-- under the repository's owner — and who maintains the repository, and so
-- clears a blocker, are the chart's. Its home refreshes it when what it reads
-- moves past what it was judged at — its row's and its conversation's
-- versions, its head's checks (0089) — and every one of those is moved by a
-- record of the log. The chart is moved by none: a configuration apply that
-- lowered a seat's tier below review took that seat's approval away, and a
-- change stamped ready on it stayed ready, and out of
-- code_mergeability_waits_idx while its first unmet precondition came to
-- wait on a person alone, until something else moved one of its versions.
--
-- So the row now also remembers the chart that judged it: chart_epoch, the
-- activation of the deciding node's chart (configplane.ActivationStamp, Unix
-- milliseconds; 0 for a chart of no known activation), stamped by the writer
-- as the versions are (Mergeability.ChartEpoch). A home names its own chart's
-- activation in its `changes_due` read, and a change whose answer an earlier
-- chart judged is due a refresh: a chart apply re-judges every live change of
-- the company once, a record each.
--
-- AND WHO CLEARS THE ANSWER: the record that closes or merges a change — a
-- close, its home's landing, a push that merges it or deletes its source, a
-- merge, a queue's landing, each on the repository's subject — writes ready
-- and waits_person false in the same apply, and a mergeability that lands
-- after the change ended stores them false: an ended change is ready for
-- nobody and waits on nobody, and no record of its home's refreshes it, so
-- an answer left standing would keep it in code_mergeability_waits_idx, which
-- needs=person walks, until its repository's purge. The index holds the
-- changes waiting now, and nothing of the company's history.
--
-- CODE_MERGEABILITY IS CREATED AGAIN, EMPTY, RATHER THAN ALTERED, for 0078's
-- reason: the due listing reads chart_epoch of every live change on every
-- home pass, and a column ALTER TABLE adds goes after the conflicted paths,
-- the row's wide column, where reading it would read each row's overflow
-- chain. No release carries 0089: the code domain is on no build of main and
-- in no image or tag (the domain's stated condition, code.RecordVersion), so
-- no store holds a row of it outside a development branch, and a change whose
-- row is gone is due its merge again at its home's next pass — a function of
-- two commits, recorded again. 0089 IS NOT EDITED: `schema_migrations` keys on
-- the filename. NO DEFAULT on chart_epoch: a writer states it, 0 for a chart
-- of no known activation.
--
-- Class: code_mergeability stays reproducible: written only by the applier —
-- from the records on a change's own subject, its answer cleared by the
-- records on its repository's subject that end the change — and removed only
-- by its repository's purge, a change's mergeability before its row.
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
    chart_epoch       INTEGER NOT NULL,
    ready             INTEGER NOT NULL,
    waits_person      INTEGER NOT NULL,
    conflicts         TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX code_mergeability_waits_idx ON code_mergeability (waits_person, change_id) WHERE waits_person = 1;    -- the changes waiting on a person, newest first: needs=person
