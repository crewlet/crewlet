-- A seat's day keeps the exact sum of its turns' durations beside their histogram, so how busy a seat was is a measured number rather than one read off the histogram's bins.
-- WHO HAS TO AGREE ON IT: every data node, identically; copied from a seat record's `turns.busy_ms` by the usage applier, and only a record at version 8 (usage.RecordVersion) carries it, so a build before it retains such a record rather than applies it.
-- NULL is a day whose record did not carry it: every row written before this column, and every record a build before version 8 published. A reader says such a day's busy time is unknown, never zero.
ALTER TABLE usage_turns ADD COLUMN busy_ms INTEGER;
