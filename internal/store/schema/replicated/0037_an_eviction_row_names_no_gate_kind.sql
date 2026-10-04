-- An eviction row no longer says which gate wrote it: there is only one.
--
-- 0034 gave `tracker_evictions` and `pages_evictions` a `kind` column, so a row
-- could tell an operator's EVICTION from a node's own RELEASE of a log as it
-- left a partition of the replicated estate. The estate is no longer divided:
-- every data node holds all of it from boot and none leaves while it runs, so
-- nothing publishes a release, no build reads the column, and the only value
-- it can hold is the 'eviction' its default writes. A column every row holds
-- the same word in is one each reader has to remember not to filter on, and
-- its CHECK still names a gate that does not exist — so it goes, and both
-- tables return to the shape they had before 0034.
--
-- 0034 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change. Every row in either table is an
-- eviction — no build ever published a release outside its own tests, since
-- nothing else called the writers — so dropping the column loses nothing.
ALTER TABLE tracker_evictions DROP COLUMN kind;
ALTER TABLE pages_evictions DROP COLUMN kind;
