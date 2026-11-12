-- A revision's facts name the human paths holding it.
--
-- Which of a merge gate's human paths hold a change's head revision to a
-- person is a GLOB MATCH: up to gitlet.MaxTargetHumanPaths globs — every rule
-- matching the target, 32 each, and the checks' own directory — against the
-- up to 1 000 paths a revision's facts list. The gate matched them on every
-- judgment: every read of a change, every row of a listing and every round of
-- an enqueue's and a mergeability's decide, inside the repository's one
-- committer, at up to the costliest glob's 3.2 ms against a whole touched list
-- each. It is the cost the path owners' match is kept off every decide for,
-- and it is now kept off the same way: the repository's HOME matches them
-- once a revision, beside the owners, and the `revise` record carries the
-- answer (gitlet.Revise.HumanPaths, gitlet/humanpaths.go).
--
-- code_repos gains human_paths_digest: the digest of the repository's rules'
-- human paths (gitlet.HumanPathsDigest — every gated rule's pattern and the
-- human paths it holds a target to, the checks' directory among them where it
-- requires checks and does not set agents_edit_checks), written by the
-- applier beside the code_protections rows by every record that writes them,
-- as 0134 writes owners_digest beside the path owners. THE DEFAULT IS '' for
-- 0134's reason: every record that writes the rules writes the column.
--
-- code_revision_facts gains two columns:
--   - human_paths_digest is the digest the facts' match was made against. The
--     gate judges human_approval only on a match whose digest is its
--     repository's — COMPUTING otherwise — and the home's due listing names a
--     change whose head revision's match is of another digest due its facts
--     again, as it names one whose owners' match is: the rules changed since.
--   - human_paths is the match, as JSON (gitlet.HumanPathMatch): each human path
--     holding the revision and each its unless waived, by its PLACE in the
--     target's human paths in their one order, which the digest pins. '' is
--     no entry matched.
--
-- And no reader of the gate reads `touched` any more: whether every path a
-- revision touches is of a class approvals_unless exempts is counted from
-- touched_count and the classes, each path listed under one class once.
--
-- CODE_REVISION_FACTS IS CREATED AGAIN, EMPTY, RATHER THAN ALTERED, for 0108's
-- reason: the due listing reads human_paths_digest of every live change's head
-- revision on every home pass, which past `touched` would read each row's
-- overflow chain, so it sits among the narrow columns, and human_paths — a
-- few bytes a place — among the wide ones, before `touched`. No release
-- carries the code domain (its stated condition, gitlet.RecordVersion), so no
-- store outside a development branch holds a row of it, and a live change
-- whose facts are gone is due them again at its home's next pass. 0108 IS
-- NOT EDITED: `schema_migrations` keys on the filename.
--
-- NO INDEX beyond the key, as in 0102: every reader reaches a revision's
-- facts through its change, and a repository's row through its key.
--
-- Class: code_repos and code_revision_facts stay reproducible: written only
-- by the applier, from the records on a repository's and a change's own
-- subjects, and removed only by the repository's purge.
ALTER TABLE code_repos ADD COLUMN human_paths_digest TEXT NOT NULL DEFAULT '';

DROP TABLE code_revision_facts;

CREATE TABLE code_revision_facts (
    change_id          TEXT    NOT NULL,
    n                  INTEGER NOT NULL,
    target_ref         TEXT    NOT NULL,
    target_oid         TEXT    NOT NULL,
    target_position    INTEGER NOT NULL,
    base_oid           TEXT    NOT NULL DEFAULT '',
    base_tree          TEXT    NOT NULL,
    head_tree          TEXT    NOT NULL,
    patch_id           TEXT    NOT NULL DEFAULT '',
    patch_id_omitted   INTEGER NOT NULL DEFAULT 0,
    touched_count      INTEGER NOT NULL,
    touches_all        INTEGER NOT NULL DEFAULT 0,
    node               TEXT    NOT NULL,
    owners_digest      TEXT    NOT NULL,
    human_paths_digest TEXT    NOT NULL,
    bytes              INTEGER NOT NULL,
    position           INTEGER NOT NULL,
    owners             TEXT    NOT NULL DEFAULT '',
    skipped            TEXT    NOT NULL DEFAULT '',
    human_paths        TEXT    NOT NULL DEFAULT '',
    classes            TEXT    NOT NULL,
    touched            TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (change_id, n)
);
