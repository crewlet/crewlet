-- Who named an assistant conversation: 'model' for a title the engine wrote,
-- 'person' for one its owner set, '' before it has any.
-- WHO HAS TO AGREE ON IT: every data node, derived from the assistant log by its one applier.
ALTER TABLE assistant_conversations ADD COLUMN title_set_by TEXT NOT NULL DEFAULT '';
