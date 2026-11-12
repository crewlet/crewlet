-- A target's tip work is owed on one row, and a landing's green is its green
-- row's.
--
-- 0110 gave every landing a green flag, and a green record set it on every
-- landing between the branch's old green row and its new one, so the record's
-- rows grew with what lay above the green: past 2 000 of them a green was
-- refused record_full, the home only ever proposes one on its newest landing,
-- and a target that gathered that many unproven landings behind a red tip
-- never greened again. 0124 then listed the repositories owing tip work by an
-- index on those flags, so its listing read every unjudged landing of the
-- fleet where it needed each target once.
--
-- A landing is now green BY ITS POSITION: at or below its branch's green row
-- (code_green.position), which a green record moves on its own, one row
-- whatever lies above it. The flag goes, and with it 0124's index.
--
-- code_tips_due is one row a protected branch holding a landing above its
-- green row that no bisect found broken: the tip work its repository's home
-- owes it (internal/gitlet's tipwork.go). A landing no record proved inserts
-- it; a green row moved, or a broken landing named, removes it once nothing
-- unbroken lies above the green. The home's listings walk it — the
-- repositories owing tip work and one repository's targets — a row a target,
-- whatever its backlog.
--
-- REBUILT RATHER THAN `ALTER TABLE … DROP COLUMN`, which re-parses every
-- trigger in the file (node 0045), as 0133 rebuilt code_notifications: the
-- old table renamed aside, the new one created under the name — so its key's
-- autoindex is named for it — every row carried over but the flag. 0110 and
-- 0124 ARE NOT EDITED: `schema_migrations` keys on the filename. The rows
-- code_tips_due derives from the landings and the green rows are derived here
-- as the applier would have written them.
--
-- Class: code_landings stays REPRODUCIBLE (0110), and code_tips_due is
-- REPRODUCIBLE: written only by the applier, from the records on a
-- repository's own subject, and removed only by those records and by the
-- repository's purge.
DROP INDEX code_landings_unjudged_idx;
DROP INDEX code_landings_change_idx;

ALTER TABLE code_landings RENAME TO code_landings_flagged;

CREATE TABLE code_landings (
    repo_id     TEXT    NOT NULL,
    target_ref  TEXT    NOT NULL,
    position    INTEGER NOT NULL,
    ordinal     INTEGER NOT NULL,
    kind        TEXT    NOT NULL,
    change_id   TEXT    NOT NULL DEFAULT '',
    entry_id    TEXT    NOT NULL DEFAULT '',
    base_oid    TEXT    NOT NULL,
    commit_oid  TEXT    NOT NULL,
    enqueued_by TEXT    NOT NULL DEFAULT '',
    merged_by   TEXT    NOT NULL DEFAULT '',
    broken      INTEGER NOT NULL DEFAULT 0,
    at          INTEGER NOT NULL,
    PRIMARY KEY (repo_id, target_ref, position, ordinal)
);

INSERT INTO code_landings (repo_id, target_ref, position, ordinal, kind, change_id, entry_id, base_oid,
    commit_oid, enqueued_by, merged_by, broken, at)
SELECT repo_id, target_ref, position, ordinal, kind, change_id, entry_id, base_oid, commit_oid, enqueued_by,
    merged_by, broken, at
FROM code_landings_flagged;

DROP TABLE code_landings_flagged;

CREATE INDEX code_landings_change_idx ON code_landings (change_id)
    WHERE change_id != '';    -- a change's landings: a broken landing's authors, the change's page

CREATE TABLE code_tips_due (
    repo_id    TEXT NOT NULL,
    target_ref TEXT NOT NULL,
    PRIMARY KEY (repo_id, target_ref)
);

INSERT INTO code_tips_due (repo_id, target_ref)
SELECT DISTINCT l.repo_id, l.target_ref FROM code_landings AS l
WHERE l.broken = 0 AND l.position > COALESCE((SELECT g.position FROM code_green AS g
    WHERE g.repo_id = l.repo_id AND g.target_ref = l.target_ref), -1);
