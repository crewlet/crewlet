-- A revision's facts name its change classes.
--
-- What a revision's change IS, to a reader that could skip it, is recorded
-- beside the facts its home read (0074), by the same `revise` record: what a
-- merge gate's exemptions read (a protection's approvals_unless and its
-- human paths' unless), and what carries an approval from one revision to the
-- next where its patch id cannot (code.ReadinessOf).
--
-- code_revision_facts gains three columns:
--   - base_tree and head_tree are the trees of the revision's base — the
--     empty tree's id for a history that shares no commit with its target's —
--     and of its head. Two revisions whose rows name the same pair hold the
--     same code, byte for byte: an approval of the one carries to the other
--     (code.ChangeClassNoCodeChange, a revision carry-forward kind), however
--     their histories differ and whatever their patch ids say, an omitted one
--     included — which an equal patch id, the other kind, cannot do. The
--     TREES are recorded and the class is not: no_code_change is a relation
--     between two revisions, which a reader of the two rows judges in its own
--     snapshot, as it compares two patch ids, where a class the home judged
--     against the row it read of the revision before would stand after a late
--     record of that revision replaced the row.
--   - classes is the PATH classes of the touched files' changes, as JSON
--     (code.RevisionClasses): each path a class holds of — whitespace_only or
--     comment_only, as the language's own lexer judges a Go file changed in
--     place (code.ClassifyGo) — named by its place in `touched`, counted from
--     0, in the list of its narrowest class; and whether the classification
--     was complete, which is what lets a reader take a path no list names as
--     one of no class rather than one never lexed. '' is no class at all, the
--     classification incomplete.
--
-- CODE_REVISION_FACTS IS CREATED AGAIN, EMPTY, RATHER THAN ALTERED, for
-- 0078's reason: every column ALTER TABLE adds goes after `touched`, the wide
-- column a list of up to code.MaxTouchedPaths paths makes — and a change's
-- readiness, judged on every read of it and every row of a listing, reads
-- the two trees of its head revision and of each revision an approval stands
-- on, which past `touched` would read each row's whole overflow chain. So the
-- trees sit among the narrow columns, and `classes` — wide, but at most some
-- four kilobytes where `touched` is up to 48 — among the wide ones, before
-- `touched`. No release carries 0074: the code domain is on no build of main
-- and in no image or tag (the domain's stated condition, code.RecordVersion),
-- so no store anywhere holds a row of it outside a development branch, and a
-- live change whose facts are gone is due them again at its home's next pass
-- (code.ReadChangesDue: a head revision with no facts) — a function of two
-- commits, recorded again. 0074 IS NOT EDITED: `schema_migrations` keys on
-- the filename, so a store that applied it would never see the change.
--
-- NO DEFAULTS on the new columns: every revise states its trees and its
-- classes, so no row reads as two revisions holding the same code, or as
-- classes nobody lexed, by an omission.
--
-- NO INDEX beyond the key, as in 0074: every reader reaches a revision's
-- facts through its change.
--
-- Class: code_revision_facts stays reproducible: written only by the
-- applier, from the records on a change's own subject, and removed only by
-- its repository's purge, a change's facts before its row.
DROP TABLE code_revision_facts;

CREATE TABLE code_revision_facts (
    change_id        TEXT    NOT NULL,
    n                INTEGER NOT NULL,
    target_ref       TEXT    NOT NULL,
    target_oid       TEXT    NOT NULL,
    target_position  INTEGER NOT NULL,
    base_oid         TEXT    NOT NULL DEFAULT '',
    base_tree        TEXT    NOT NULL,
    head_tree        TEXT    NOT NULL,
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
    classes          TEXT    NOT NULL,
    touched          TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (change_id, n)
);
