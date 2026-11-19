-- A project says how long an item in progress may go unchanged before it reads stuck.
-- WHO HAS TO AGREE ON IT: every data node, identically; copied from the project document's `stuck_after_hours` by the object apply, as `target_date` is (0027), and only a record at version 19 (worklet.RecordVersion) carries it, so a build before it retains such a record rather than applies it.
-- NULL is no window set, which the item flow reads as its default of 8 hours; no project carries one yet (the field is new), so nothing needs filling.
-- No index: the flow reads it by the project's own key, one seek per item in progress.
ALTER TABLE tracker_projects ADD COLUMN stuck_after_hours INTEGER;
