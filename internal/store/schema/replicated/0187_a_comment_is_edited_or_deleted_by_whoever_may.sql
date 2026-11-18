-- A change's comment is edited by its author, or deleted, leaving a tombstone in its thread.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; only a comment_edit or comment_delete record (gitlet.CommentEditsVersion) fills them, and a build before it retains such a record rather than applies it, so a row applied before this migration reads its defaults on every node.
-- About 4 bytes a comment row (three zeros and an empty string on a comment never edited or deleted); a delete clears the body, and no index reads them.
-- edited_at is the last edit's broker instant, 0 for none; deleted_at the delete's, 0 for a comment that stands; deleted_by and deleted_by_kind who deleted it.
-- Class: code_comments stays reproducible (gitlet.ReproducibleTables).
ALTER TABLE code_comments ADD COLUMN edited_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_comments ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_comments ADD COLUMN deleted_by TEXT NOT NULL DEFAULT '';
ALTER TABLE code_comments ADD COLUMN deleted_by_kind TEXT NOT NULL DEFAULT '';
