-- A revision keeps how its push moved its branch.
--
-- WHO HAS TO AGREE ON IT: every data node, identically (gitlet.ReproducibleTables); code_revisions
-- stays reproducible, written only by the applier from the push records on the repository's
-- subject.
--
-- A change's timeline says what made each revision: a push forward, a force-push, or the engine's
-- rebase of the change onto its target. via is the push's way (gitlet.PushVia: git, engine), and
-- forced whether its command rewrote the branch rather than moving it forward (gitlet.Command's
-- Forced), both off the push entry that moved the change's head, in the base format. The engine's
-- one write that rewrites a branch is a rebase (gitlet.WriteRebaseChange), so a revision an engine
-- push forced is the change rebased. '' and 0 on a revision no push made (an open by a tool, a
-- reopen) and on every revision applied before this migration ran: no build that wrote a revision
-- before this one has shipped. About 10 bytes a revision; no index reads them.
ALTER TABLE code_revisions ADD COLUMN via TEXT NOT NULL DEFAULT '';
ALTER TABLE code_revisions ADD COLUMN forced INTEGER NOT NULL DEFAULT 0;
