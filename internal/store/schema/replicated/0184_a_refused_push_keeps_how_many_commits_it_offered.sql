-- A push a rule refused keeps how many commits it offered.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; only a push entry carrying gitlet.OfferedVersion's field fills it, and a build before it retains such a record rather than applies it, so a row applied before this migration reads 0 on every node.
-- About 2 bytes a push row (an integer, 0 on every row but an attempt's that offered commits); no record writes a row it did not write before, and no index reads it.
-- Class: code_pushes stays reproducible (gitlet.ReproducibleTables).
ALTER TABLE code_pushes ADD COLUMN offered INTEGER NOT NULL DEFAULT 0;
