-- An adoption row no longer says whether its ledger loss was folded.
--
-- 0030 gave `statelog_adoption` a `ledger_folded` column so the engine could
-- carry, once at boot, the adoptions a build from before the operation ledger
-- travelled had recorded into that ledger's watermark. No such build is
-- supported: every row this build writes is an adoption whose ledger travelled
-- with its watermark, so the fold had nothing to carry and is gone, and a
-- column nothing reads is a claim that some reader still cares. The table is
-- this node's adoption HISTORY and nothing else.
--
-- REBUILT RATHER THAN `ALTER TABLE … DROP COLUMN`, because the driver cannot
-- drop a column in this estate any more: a DROP COLUMN re-parses every trigger
-- in the file, and the memory change-sequence triggers of 0041 name
-- `NEW.rowid`, which that re-parse refuses ("no such column: rowid") whatever
-- table the column is dropped from. A table copied, dropped and renamed is
-- never re-parsed against them. The new table is the one 0022 declared, and
-- its rows are copied whole but for the column that goes.
--
-- 0030 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change.
CREATE TABLE statelog_adoption_rebuilt (
    started_at   INTEGER NOT NULL PRIMARY KEY,
    donor        TEXT    NOT NULL,
    manifest     TEXT    NOT NULL,
    completed_at INTEGER
);

INSERT INTO statelog_adoption_rebuilt (started_at, donor, manifest, completed_at)
SELECT started_at, donor, manifest, completed_at FROM statelog_adoption;

DROP TABLE statelog_adoption;

ALTER TABLE statelog_adoption_rebuilt RENAME TO statelog_adoption;
