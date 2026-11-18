-- A closed change keeps who closed it.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; code_changes stays reproducible
-- (gitlet.ReproducibleTables), written only by the applier from the records on the repository's
-- subject. closed_by is who closed it, as merged_by is who merged it (0114): a seat by its handle
-- or a person by the human seat they hold, closed_by_kind which (agent or person,
-- gitlet.PartyKinds). A close record's principal names them (gitlet.ChangeClose.By, in the base
-- format), and a push that closes a change by deleting its source names its pusher, '' for a push
-- nobody made (an import's or a mirror's). A reopen clears both, with the close's reason and time.
-- A change closed on a node before it ran this migration reads '' on that node, as every column
-- added to a written table does: no build that wrote a change before this one has shipped.
-- About 20 bytes a closed change, '' on every other; no index reads them, and a close and a
-- reopen write them in the write that moves the state already
-- (TestAChangeWriteRewritesOnlyTheIndexesItMoves).
ALTER TABLE code_changes ADD COLUMN closed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE code_changes ADD COLUMN closed_by_kind TEXT NOT NULL DEFAULT '';
