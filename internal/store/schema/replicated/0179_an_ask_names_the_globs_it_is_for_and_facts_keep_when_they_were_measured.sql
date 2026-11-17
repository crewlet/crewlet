-- An ask names the globs it is for, and a revision's facts keep when they were measured.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; only a revise carrying gitlet.OwnedPathsVersion's fields fills these columns, and a build before it retains such a record rather than applies it, so a row applied before this migration reads 0 and '' on every node.
-- About 8 bytes a revision's facts a home of this build records (measured_at, after the wide columns: read only beside touched), and on a path owner's ask the JSON of its globs, at most gitlet.MaxOwnedPathsBytes and typically 20 to 60 bytes, a row holding them or an asker's note and never both; no record writes a row it did not write before. code_revision_facts.owners, which the merge gate reads on every judgment, keeps each matched owner's first glob and how many more beside it, never the list: at most about 56 KB for 64 owners at the widest against 5.7 KiB of owners alone, about 50 bytes more an owner typically.
-- Class: code_revision_facts and code_reviewers stay reproducible.
ALTER TABLE code_revision_facts ADD COLUMN measured_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_reviewers ADD COLUMN owned_paths TEXT NOT NULL DEFAULT '';
ALTER TABLE code_reviewers ADD COLUMN owned_paths_omitted INTEGER NOT NULL DEFAULT 0;
