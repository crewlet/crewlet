-- A conversation's sealed summary of its first summary_through messages (assistant record v2).
-- WHO HAS TO AGREE ON IT: every data node, derived from the assistant log by its one applier.
ALTER TABLE assistant_conversations ADD COLUMN summary_sealed TEXT NOT NULL DEFAULT '';
ALTER TABLE assistant_conversations ADD COLUMN summary_through INTEGER NOT NULL DEFAULT 0;
