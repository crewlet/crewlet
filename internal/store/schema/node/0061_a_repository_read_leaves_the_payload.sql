-- A read of a unit repository is counted by columns of its own.
--
-- `code_fetched` is one read of a `visibility: unit` repository's contents — a
-- git fetch that carried a pack, a code tool's answer — and the usage
-- publisher folds a day of them into who read each repository (internal/usage,
-- kind `repo_read`): by repository, principal kind, principal, way and seat,
-- their reads counted and their bytes summed. The publisher derives today and
-- yesterday again on every fifteen-second tick either moves, so that grouping
-- is paid on every such tick, over every read of the day.
--
-- READ OUT OF THE PAYLOAD, IT COST MORE THAN THE DAY IT WAS PART OF. Six
-- `json_extract`s a row, each parsing the row's JSON again: measured with
-- store.BenchmarkUsageForDay (`-benchtime 5x -count 1`, two cores of an Intel
-- Core Ultra 7 265U), a busy day of every other usage type derived in 62 ms
-- and the same day beside fifty thousand reads of unit repositories in 334 ms —
-- 272 ms the reads alone, past the 150 ms a derived day may add (1 % of the
-- tick; M3-decisions M3-O22). Each extraction measured about 50 ms over the
-- fifty thousand, and grouping on columns 23 ms, so the fields the statement
-- groups and sums on are columns here, filled when the row is written — one
-- decode of a quarter-kilobyte record, once — and the statement reads no JSON
-- at all: on these columns the same day derives in 151 ms, the reads adding
-- 86 ms. The repository's NAME, a label rather than a key, stays in the
-- payload: it is read once per repository, off the newest read's row by its
-- instant (store.DB.usageRepoReads).
--
-- `read_` LIKE `spend_stage`: each column belongs to one record's meaning, a
-- repository read's, and is empty on every other row — a byte a row, which no
-- reader of another type has to know is there; a side table keyed on the
-- row's identity would cost a second insert, a join and a sweep kept in step
-- with this one, which is node/0015's reasoning and holds here.
--
-- WHO HAS TO AGREE ON IT: this node alone, like the rows it describes — the
-- event log is the node's own estate (ADR-0021).
--
-- NOTHING IS BACKFILLED: `code_fetched` is new with these columns, so no row
-- of it was written before them anywhere.
ALTER TABLE crewlet_events ADD COLUMN read_repo_id        TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN read_principal_kind TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN read_principal      TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN read_via            TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN read_seat           TEXT    NOT NULL DEFAULT '';
ALTER TABLE crewlet_events ADD COLUMN read_bytes          INTEGER NOT NULL DEFAULT 0;
