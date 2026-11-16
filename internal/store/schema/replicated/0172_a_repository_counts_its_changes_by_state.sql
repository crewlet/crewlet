-- A repository counts its changes by state, and the repositories in use are tallied by who may read them.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; the applier keeps the counts and re-derives them (gitlet.DerivationVersion), so this file fills nothing.
-- Four integers a repository and about 100 bytes a tally row; a record that moves a change's state moves its repository's row and one tally row, a settings change that moves its owner or visibility two, a delete or a restore one.
-- Class: code_repos stays reproducible, and code_change_tallies is reproducible (gitlet.ReproducibleTables).

ALTER TABLE code_repos ADD COLUMN draft_changes  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN open_changes   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN closed_changes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN merged_changes INTEGER NOT NULL DEFAULT 0;

CREATE TABLE code_change_tallies (
    visibility     TEXT    NOT NULL,
    owner_kind     TEXT    NOT NULL,
    owner          TEXT    NOT NULL,
    draft_changes  INTEGER NOT NULL,
    open_changes   INTEGER NOT NULL,
    closed_changes INTEGER NOT NULL,
    merged_changes INTEGER NOT NULL,
    PRIMARY KEY (visibility, owner_kind, owner)
);
