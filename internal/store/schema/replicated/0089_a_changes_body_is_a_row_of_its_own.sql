-- A change's body is a row of its own, and its change weighs it.
--
-- 0081 kept a change's body — its description, up to code.MaxChangeBodyBytes,
-- 32 KiB — in code_changes, declared before every column a head move reads
-- and rewrites, and a purge counted the row as it counts a ref. Both cost the
-- same thing: a row that does not fit its page keeps its start in the leaf and
-- the rest in a chain of overflow pages, and this driver reads a row WHOLE,
-- chain and all, whichever of its columns a statement asks for (0003, 0033).
--
--   - EVERY HEAD MOVE PAID FOR THE BODY. A push that moves a live change's
--     head reads the change's head_oid, revision and version and rewrites
--     them with its position, to record the next revision. Measured at the
--     review that moved the body (five hundred rows, three rounds of one run,
--     no race detector, two E-cores of a Core Ultra 7 265U), the read took
--     74 µs over a row with no body, 166 µs over one carrying 32 KiB where
--     0081 put it and 109 µs with the body declared last, and the rewrite
--     108 µs, 226 µs and 170 µs: on every revision of a change with a long
--     description, on every data node, and on every row a listing reads.
--   - A PURGE PART NEVER WEIGHED IT. A part took a repository's changes by
--     their rows alone, about a thousand of them — 32 MB of bodies at the
--     bound — on top of push rows just short of code.PurgePartBytes: 112 ms
--     of the write lane in the same review's run of BenchmarkApplyAPurgePart,
--     against 64 ms for the same rows without bodies, where that bound sizes
--     the most a part takes to a third of the 250 ms a record may hold the
--     lane.
--
-- SO THE BODY IS A ROW OF ITS OWN, code_change_bodies, read by its change's
-- key: written by the records on the repository's subject that write a
-- change's text — the one that opens it, and an edit that sets its body,
-- which deletes the row when it sets none — only where the body is not
-- empty, and removed with its change. What is left on code_changes is
-- narrow, and its writers keep it so: refs, oids and handles, the work item's
-- names, a title of at most 256 bytes and a close reason of at most 1 KiB. A
-- row stays inside its page, and a head move, a listing and a purge's walk
-- each read one page of it.
--
-- AND THE CHANGE WEIGHS ITS BODY: body_bytes is the body's bytes as written,
-- 0 where there is none, set by every record that writes the body. On the
-- change's row rather than the body's, because a purge walks the changes, and
-- a width read off the body's own row would walk the body's overflow pages to
-- learn what deleting them costs. A purge part takes whole changes only while
-- what its wide rows weigh — the push rows by their row_bytes, then the
-- changes by their body_bytes — is under code.PurgePartBytes, at most one
-- change past it, and the final purge is formed only once both fit. NO
-- DEFAULT, as code_pushes.row_bytes has none: a writer states the width of
-- every change it stores, so no change reads as free by omission.
--
-- CREATED AGAIN, EMPTY, RATHER THAN ALTERED: no release has shipped 0081, and
-- nothing writes a change before the records that open one, which no build
-- carries yet, so no store holds a row of it — and a NOT NULL column without
-- a default is one ALTER TABLE cannot add. 0081 IS NOT EDITED:
-- `schema_migrations` keys on the filename, so a store that applied it would
-- never see the change. Dropping the table takes its seven indexes with it,
-- and each is created again below, unchanged.
--
-- Class: code_changes and code_change_bodies are reproducible: written only by
-- the applier, from the records on the repository's subject, and removed only
-- by the repository's purge, a change's body with its revisions and authors
-- before the change's own row.
DROP TABLE code_changes;

CREATE TABLE code_changes (
    id                TEXT    NOT NULL PRIMARY KEY,
    repo_id           TEXT    NOT NULL,
    number            INTEGER NOT NULL,
    title             TEXT    NOT NULL,
    body_bytes        INTEGER NOT NULL,
    state             TEXT    NOT NULL,
    source_ref        TEXT    NOT NULL,
    target_ref        TEXT    NOT NULL,
    head_oid          TEXT    NOT NULL,
    revision          INTEGER NOT NULL,
    owner             TEXT    NOT NULL,
    owner_kind        TEXT    NOT NULL,
    owner_operator_id TEXT    NOT NULL DEFAULT '',
    work_backend      TEXT    NOT NULL DEFAULT '',
    work_id           TEXT    NOT NULL DEFAULT '',
    work_key          TEXT    NOT NULL DEFAULT '',
    work_project      TEXT    NOT NULL DEFAULT '',
    created_turn      TEXT    NOT NULL DEFAULT '',
    created_run       TEXT    NOT NULL DEFAULT '',
    opened_at         INTEGER NOT NULL,
    opened_position   INTEGER NOT NULL,
    closed_at         INTEGER,
    close_reason      TEXT    NOT NULL DEFAULT '',
    merged_at         INTEGER,
    merged_via        TEXT    NOT NULL DEFAULT '',
    merge_oid         TEXT    NOT NULL DEFAULT '',
    version           INTEGER NOT NULL,
    position          INTEGER NOT NULL
);

CREATE INDEX code_changes_number_idx ON code_changes (repo_id, number);    -- a change by its number (web!42), a repository's changes in number order, and a purge's walk to each change's revisions, authors and body
CREATE INDEX code_changes_open_idx ON code_changes (repo_id, source_ref) WHERE state IN ('draft', 'open');    -- the live change a source branch heads: an open's decide, the push apply's revisions and closes, the ref expiry's exemption, a run's delivered refs and the home's due listing
CREATE INDEX code_changes_target_idx ON code_changes (repo_id, target_ref, head_oid) WHERE state IN ('draft', 'open');    -- the live changes into a target: the push apply's merge of a change whose head its target is pushed to, and the home's recheck when a target moves
CREATE INDEX code_changes_state_idx ON code_changes (state, opened_position, id);    -- the company's changes in a state, newest first
CREATE INDEX code_changes_owner_idx ON code_changes (owner, state, opened_position, id);    -- an owner's changes in a state, newest first
CREATE INDEX code_changes_work_idx ON code_changes (work_backend, work_id) WHERE work_id != '';    -- a work item's changes
CREATE INDEX code_changes_merge_idx ON code_changes (repo_id, merge_oid) WHERE merge_oid != '';    -- the change a commit merged: a commit's page names it

CREATE TABLE code_change_bodies (
    change_id TEXT NOT NULL PRIMARY KEY,
    body      TEXT NOT NULL
);
