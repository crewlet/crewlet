-- An open ask says when the facts have overtaken it: superseded, when its asker
-- asked the same person again on the same item, or moot, when the item is done.
--
-- WHO HAS TO AGREE ON IT: every data node, identically. `overtaken` is a derived
-- column (worklet's apply_derived.go): the applier maintains it and the
-- derivation version bump fills it, so this migration adds the column alone.
ALTER TABLE tracker_comments ADD COLUMN overtaken TEXT NOT NULL DEFAULT '';
