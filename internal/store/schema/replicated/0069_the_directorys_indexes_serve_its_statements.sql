-- The identity estate's indexes are held to the statements that read them.
--
-- Found by planning every statement internal/iamdomain runs over the people,
-- the invitations and the trail (its plan gate,
-- TestTheDirectoryStatementsArePlanned), on an estate with no statistics, as
-- every node plans: one index no plan reached, and three reads with no index
-- behind them.
--
-- # iam_people_bucket_idx GOES
--
-- 0041 declared (bucket, id) for "the directory listing, and the sweep's own
-- walk". The listing pages through the people's own key, and the retention
-- sweep collects sessions, invitations, credentials and the trail by bucket and
-- never a person — a person's row goes only with their removal, by their key.
-- An index no plan reaches is a write on every apply of every identity record
-- on every data node, carried in every snapshot and every catch-up, for no
-- reader.
--
--
-- `/health` asks on every probe whether the company has its first person
-- (`AnyPerson`), and with no index on the kind that question read the people
-- from the heap until it met one — every row, on a company nobody has joined.
-- A seek of the kind answers it at the first entry.
--
-- # iam_history_version_idx, for the trail in log order
--
-- The trail a person reads (`/iam/history`) is newest first by POSITION, and a
-- caller's instant is resolved to a position once (`PositionAt`). Neither had
-- an index in that order, so every page read the whole trail from the heap and
-- sorted it — the trail that keeps a year of changes and every sign-in of the
-- session horizon. Walked from its newest entry, a page stops at its limit and
-- an instant at its first row at or before it.
--
-- # iam_history_person_idx is re-keyed on the position
--
-- (person_id, broker_at DESC) served one person's trail in an order nothing
-- asks for: the page orders by position, so it sought the person and then
-- sorted everything they had ever done. (person_id, version) serves the page in
-- its own order, and the removal's erasure, which seeks the person alone, as
-- before. The broker instant is not the position's order — it is the stream
-- leader's clock, which nothing holds monotone across a change of leader — so
-- the page's cursor, a position, needs the position in the index.
--
-- 0041 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change.
DROP INDEX IF EXISTS iam_people_bucket_idx;
CREATE INDEX iam_people_kind_idx ON iam_people (kind);                       -- whether anybody of a kind is enrolled
CREATE INDEX iam_history_version_idx ON iam_history (version);               -- the trail in log order, newest first
DROP INDEX IF EXISTS iam_history_person_idx;
CREATE INDEX iam_history_person_idx ON iam_history (person_id, version);     -- one person's trail, newest first
