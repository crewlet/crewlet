-- A change opened to revert another names it, so the change it reverts answers its revert.
-- WHO HAS TO AGREE ON IT: every data node, identically; only a push entry carrying gitlet.RevertsVersion's field fills it, and a build before it retains such a record rather than applies it, so a row applied before this migration reads 0 on every node.
-- Class: code_changes stays reproducible (gitlet.ReproducibleTables); about 2 bytes a change, and an index entry for a revert alone.
ALTER TABLE code_changes ADD COLUMN reverts INTEGER NOT NULL DEFAULT 0;
CREATE INDEX code_changes_reverts_idx ON code_changes (repo_id, reverts, number) WHERE reverts > 0;    -- a merged change's detail answers the latest change that reverts it (gitlet changeread.go)
