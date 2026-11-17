-- A check's ending keeps how many tests ran and how many lines its output has.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; only a check_finished record carrying gitlet.CheckCountsVersion's fields fills these columns, and a build before it retains such a record rather than applies it, so a row applied before this migration reads NULL and 0 on every node.
-- About 5 bytes a check row (NULL or 0 on every row whose ending counted nothing); no record writes a row it did not write before, and no index reads them. Appended after the wide columns: only a `check` read, which reads those whole, reads them.
-- Class: code_checks stays reproducible (gitlet.ReproducibleTables).
ALTER TABLE code_checks ADD COLUMN tests_total INTEGER;
ALTER TABLE code_checks ADD COLUMN tests_failed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_checks ADD COLUMN tests_skipped INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_checks ADD COLUMN log_lines INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_checks ADD COLUMN tail_cut INTEGER NOT NULL DEFAULT 0;
