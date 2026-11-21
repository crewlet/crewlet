-- A change's comment is edited by its author, or deleted, leaving a tombstone in its thread.
-- WHO HAS TO AGREE ON IT: every data node, identically; only a comment_edit or comment_delete record (gitlet.CommentEditsVersion) fills edited_at (the last edit's broker instant), deleted_at (the delete's), deleted_by and deleted_by_kind, and a build before it retains such a record rather than applies it, so a row applied before this migration reads its defaults on every node.
-- Class: code_comments stays reproducible (gitlet.ReproducibleTables); about 4 bytes a comment row, a delete clears the body, and no index reads them.
ALTER TABLE code_comments ADD COLUMN edited_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_comments ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_comments ADD COLUMN deleted_by TEXT NOT NULL DEFAULT '';
ALTER TABLE code_comments ADD COLUMN deleted_by_kind TEXT NOT NULL DEFAULT '';
