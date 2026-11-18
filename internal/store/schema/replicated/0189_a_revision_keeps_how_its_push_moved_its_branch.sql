-- A revision keeps how its push moved its branch.
-- WHO HAS TO AGREE ON IT: every data node, identically; written only by the applier from the push entry that moved the change's head, in the base format: via is the push's way (gitlet.PushVia: git, engine), forced whether its command rewrote the branch (gitlet.Command's Forced), so an engine push forced is a rebase (gitlet.WriteRebaseChange); '' and 0 on a revision no push made or applied before this migration.
-- Class: code_revisions stays reproducible (gitlet.ReproducibleTables); about 10 bytes a revision, and no index reads them.
ALTER TABLE code_revisions ADD COLUMN via TEXT NOT NULL DEFAULT '';
ALTER TABLE code_revisions ADD COLUMN forced INTEGER NOT NULL DEFAULT 0;
