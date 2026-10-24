-- A custody batch's rows are KEYED, one row per identity, and a settled batch
-- is remembered for as long as its rows are kept.
--
-- WHO HAS TO AGREE ON IT: this node alone, and only about rows it wrote — the
-- same answer 0038 gave `custody_unsettled`. Which data node keeps a batch is
-- the company's answer and lives in coordination (coord.Custody); what these
-- tables remember is what THIS node wrote and what it has heard back.
--
-- `custody_unsettled_rows` — one row per identity of a batch this node has
-- written and not settled: `(row_time, row_id)` is the event log's own
-- `(event_time, event_id)`, named apart from it so a predicate written over
-- the log's columns unqualified — every count's own (unsettled.go) — reads
-- the same beside this table as beside the log alone. 0038 held them as a JSON
-- array on the batch's own row, which the fleet reads unpacked with
-- `json_each`. A usage count leaving a batch in flight out over that array —
-- a `NOT IN` over `json_each` — would check each row it counted against the
-- whole list one entry at a time: one usage fingerprint over a
-- five-thousand-row day took 0.3 s beside a thousand in-flight identities and
-- 2.4 s beside ten thousand (measured) — and a batch whose claim fails stays in
-- flight until the keeper's next pass settles it, so a coordination store away
-- for a few minutes leaves tens of thousands. The
-- KEY IS THE LOG'S OWN IDENTITY, leading, so "is this row in flight" is a seek
-- of it from the counted row (internal/store/unsettled.go); the batch closes
-- it, because two batches may name one row. The batch index is what a settle
-- finds its own identities by.
--
-- `custody_settled` — a batch this node SETTLED, kept or not, and when. A
-- group redelivers a batch whose acknowledgement was lost, to whichever member
-- asks next, and that can be this node after it settled the batch: written
-- again, the batch would be in flight again, and the kept rows every usage
-- count had published would leave every count until the claim answered a
-- second time — a day published short in the meantime, over good history on
-- every node. So a
-- write of a batch this node already settled writes nothing: kept, its rows
-- are here and counting; not kept, the fleet's copy is another node's. Swept
-- with the event log (store.EventRetention): past it the rows the record
-- speaks for are gone too.
--
-- `custody_unsettled` is REBUILT without its `events` column, whose list the
-- new table now holds — a copy beside it would be a second idea of which rows
-- a batch has, and nothing reads it. Rebuilt rather than `DROP COLUMN` for
-- 0045's reason: a DROP COLUMN re-parses every trigger in the file, and the
-- memory change-sequence triggers of 0041 refuse that re-parse. 0038 IS NOT
-- EDITED, because `schema_migrations` keys on the filename.
--
-- The rows a node wrote before this migration are carried across: every entry
-- of each batch's list becomes a row, and an entry that names no row — one
-- missing either half, which no writer ever made — is dropped rather than
-- carried as a key nothing in the log has.
CREATE TABLE custody_unsettled_rows (
    row_time INTEGER NOT NULL,
    row_id   TEXT    NOT NULL,
    batch_id TEXT    NOT NULL,
    PRIMARY KEY (row_time, row_id, batch_id)
);

CREATE INDEX custody_unsettled_rows_batch_idx ON custody_unsettled_rows (batch_id);

INSERT INTO custody_unsettled_rows (row_time, row_id, batch_id)
SELECT json_extract(j.value, '$.t'), json_extract(j.value, '$.id'), c.batch_id
  FROM custody_unsettled AS c, json_each(c.events) AS j
 WHERE json_extract(j.value, '$.t') IS NOT NULL
   AND json_extract(j.value, '$.id') IS NOT NULL
ON CONFLICT DO NOTHING;

CREATE TABLE custody_unsettled_rebuilt (
    batch_id   TEXT    NOT NULL PRIMARY KEY,
    origin     TEXT    NOT NULL,
    written_at INTEGER NOT NULL
);

INSERT INTO custody_unsettled_rebuilt (batch_id, origin, written_at)
SELECT batch_id, origin, written_at FROM custody_unsettled;

DROP TABLE custody_unsettled;

ALTER TABLE custody_unsettled_rebuilt RENAME TO custody_unsettled;

CREATE TABLE custody_settled (
    batch_id   TEXT    NOT NULL PRIMARY KEY,
    kept       INTEGER NOT NULL,
    settled_at INTEGER NOT NULL
);

CREATE INDEX custody_settled_at_idx ON custody_settled (settled_at);
