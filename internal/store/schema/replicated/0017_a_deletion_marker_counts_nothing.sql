-- A deletion marker holds no tally of the records its gate drops.
--
-- # What goes
--
-- `tracker_deletions.rejects` / `last_reject_at` and `pages_deletions.rejects`
-- / `last_reject_at`: a count of the records each marker's deletion gate
-- dropped, and when the last one was.
--
-- # Why
--
-- A gated record produces no rows. That is the contract every domain's `Gated`
-- answers under (`internal/statelog`'s Applier): a gate is a rule under which
-- a durable record applies nowhere, and a gate that counted on its marker
-- would be that record changing a row on the one path that promises it
-- changes none. Neither applier in this build writes these columns, and
-- nothing in it reads them. What an operator sees of a drop is the
-- framework's signal for every gate alike — the `statelog_record_gated` log
-- line naming the gate, the position and the writer, the
-- `crewlet.statelog.records_gated` counter, and the `records_gated` alarm that
-- counter feeds — so a count on the row would be a second answer to that
-- question, with no reader.
--
-- They go rather than stay unwritten, because a column nothing writes is read
-- by the next person who finds it as a figure that means something: a tally
-- held at whatever an older build left in it says a purged task or page
-- stopped catching redeliveries, when nothing is counting them.
--
-- # A build that predates this migration
--
-- It increments its own copy of the counters when its deletion gate drops a
-- record, on a database of its own that still has both columns; nothing reads
-- the count. When that node runs this build, this migration drops the columns
-- and the counts with them. A build that predates this migration does not run
-- on a database it has touched: running a binary below the schema it already
-- migrated is not supported (docs/guides/deployment.md).
--
-- Turso takes `ALTER TABLE … DROP COLUMN` for a column no index, view or
-- trigger names, and none names these.
--
-- 0002 AND 0005 ARE NOT EDITED. `schema_migrations` keys on the filename, so a
-- file that has already run never runs again: a fresh database and an upgraded
-- one converge here, by the same route.

ALTER TABLE tracker_deletions DROP COLUMN rejects;
ALTER TABLE tracker_deletions DROP COLUMN last_reject_at;

ALTER TABLE pages_deletions DROP COLUMN rejects;
ALTER TABLE pages_deletions DROP COLUMN last_reject_at;
