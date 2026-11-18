-- A closed change keeps who closed it.
-- WHO HAS TO AGREE ON IT: every data node, identically; written only by the applier from the records on the repository's subject: closed_by is a close record's principal (gitlet.ChangeClose.By, in the base format) or the pusher whose push deleted the source ('' for a push nobody made), closed_by_kind which (gitlet.PartyKinds); a reopen clears both, and a change closed before this migration reads ''.
-- Class: code_changes stays reproducible (gitlet.ReproducibleTables); about 20 bytes a closed change, and no index reads them.
ALTER TABLE code_changes ADD COLUMN closed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE code_changes ADD COLUMN closed_by_kind TEXT NOT NULL DEFAULT '';
