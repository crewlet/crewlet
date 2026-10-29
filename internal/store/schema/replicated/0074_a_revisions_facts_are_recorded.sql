-- A revision's facts are recorded.
--
-- What one revision of a change IS against the branch it would merge into —
-- where its history meets the target's, the files it touches, the id of the
-- change it makes — is a function of two commits alone, which the
-- repository's HOME git node reads from its cache (internal/code/query's
-- revision_facts) and records on the change: a `revise` record on the
-- change's own subject (0072), which the repository's committing data node
-- publishes for it. Every reader of a revision's facts after that reads these
-- rows, never git: an approval carried from one revision to the next on an
-- equal patch id, an interdiff kept to the files two revisions touch, a
-- change's readiness.
--
-- code_revision_facts is one row each revision of a change the home recorded
-- facts of, keyed by the change and the revision's number.
--   - target_ref, target_oid and target_position are the target branch, the
--     tip the facts were read against and the packed position of the record
--     that moved the branch there. A later record of the same revision
--     against the same branch is taken only from a later tip; one against
--     another branch — the change was retargeted since — replaces the row.
--   - base_oid is the merge base the change is measured from, '' for a
--     history that shares no commit with its target's, which is measured from
--     the empty tree.
--   - patch_id is git patch-id --verbatim of the change's cumulative diff from
--     its base: '' for a revision that changes nothing, and for one whose id
--     was not taken (patch_id_omitted: its diff past the bound on one the home
--     identifies, or out of the read's budget of git). An empty id equals
--     nothing, so no approval carries across it.
--   - touched_count and touched are the files the change touches, in git's
--     order, NUL-separated (no path of a tree holds a NUL); touches_all is a
--     change past the bound on that list, which every reader reads as every
--     path, and touched is then empty.
--   - node is the git node that recorded them: the repository's home as it
--     took itself to be, which asked for them.
--   - owners_digest, owners and skipped are its PATH-OWNER MATCH: the digest
--     of the repository's path owners the home matched the touched paths
--     against (code.OwnersDigest), the owners one of whose globs matched a
--     touched path — every owner, where it touches all — and the owners the
--     record asked no review of, with why; each list JSON. A record whose
--     snapshot holds path owners of another digest records nothing: the home
--     matches again.
--   - bytes is what the row's wide columns weigh — touched, owners and
--     skipped — so a purge part's cut of a change's facts reads the widths
--     off the rows it deletes and no wide value it keeps.
--   - position is the record that wrote the row.
-- A revision whose head its target holds already has NO ROW: whatever moved
-- the target there merged the change, which its home lands with a record on
-- the repository's subject, and a revise naming its head as its own base is
-- refused (code.Revise).
--
-- THE WIDE COLUMNS ARE DECLARED LAST: a list of up to code.MaxTouchedPaths
-- paths does not fit a leaf page, and a row read for a column before it stops
-- there (0066's measurement).
--
-- code_conversations.text_bytes counts a revision's facts beside its
-- conversation's text — every record on the change's subject moves it by what
-- it wrote of either — so a purge part weighs a change's subject's wide rows by
-- that one narrow row, as it weighs its comments and its findings' texts.
--
-- code_reviewers gains a reason beside `requested`: `path_owner`, an ask a
-- revision's facts made of the reviewer a matched path owner resolves to — a
-- unit's effective lead, or the seat itself — which no principal asked, so its
-- requested_by and requested_by_kind are ''.
--
-- NO repo_id, as on the conversation's tables: a change's facts are reached
-- through the change, by their key. NO INDEX beyond the key: the home's due
-- listing asks one revision's facts by it, a revise's decide asks whether the
-- change holds any by its prefix, and a purge reaches a change's facts by the
-- same prefix.
--
-- Class: code_revision_facts is reproducible: written only by the applier,
-- from the records on a change's own subject, and removed only by its
-- repository's purge, a change's facts before its row.
CREATE TABLE code_revision_facts (
    change_id        TEXT    NOT NULL,
    n                INTEGER NOT NULL,
    target_ref       TEXT    NOT NULL,
    target_oid       TEXT    NOT NULL,
    target_position  INTEGER NOT NULL,
    base_oid         TEXT    NOT NULL DEFAULT '',
    patch_id         TEXT    NOT NULL DEFAULT '',
    patch_id_omitted INTEGER NOT NULL DEFAULT 0,
    touched_count    INTEGER NOT NULL,
    touches_all      INTEGER NOT NULL DEFAULT 0,
    node             TEXT    NOT NULL,
    owners_digest    TEXT    NOT NULL,
    bytes            INTEGER NOT NULL,
    position         INTEGER NOT NULL,
    owners           TEXT    NOT NULL DEFAULT '',
    skipped          TEXT    NOT NULL DEFAULT '',
    touched          TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (change_id, n)
);
