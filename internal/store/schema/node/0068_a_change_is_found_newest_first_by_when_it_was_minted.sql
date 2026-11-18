-- A change is found newest first by when it was minted.
--
-- WHO HAS TO AGREE ON IT: this node alone: derived from replicated rows by this node's own walk, and rebuilt from them.
-- A document is keyed by the millisecond its change's id was minted in and five bits of the id, so every node reads the newest first; its postings are the slots doc << 16 up to (doc + 1) << 16, which an edit or a drop deletes as one range, so no term list is kept.
-- The first build walks the changes newest first by id from where it began, beside the forward walk; back_pos is where it began (-1 before it), back_floor the least id it has reached ('' once whole).
-- 0067 IS NOT EDITED: a node that applied it keeps its index under the old numbering, so its tables are dropped here and the first build walks again, newest first.
-- Measured (BenchmarkTheChangeIndexBuild, built newest first from ids minted at 40 000 a day): about 2.6 KB a change of short text, 9.9 KB at 150
-- distinct words with 10 000 indexed and 10.4 KB with 30 000, and 55 KB at a 32 KiB description; about 152 GB a year at 40 000 changes a day of 150 distinct words.
-- Its first build walks 6.9 ms a change of 150 distinct words at 10 000 indexed and 18.9 ms over 30 000, so at 40 000 a day the newest day is searchable after
-- about 14 minutes, the week after 1.9 hours, the month after 8.2 hours and the year after 101 hours, each a lower bound (24.9 ms a change past 30 000).
-- Smaller batches, which the two-second write hold takes as the index grows, cost more: 10.9 KB and 14.7 ms a change at 64 KiB, 11.1 KB and 19.7 ms at 32 KiB.
-- A new copy of the estate keeps these rows: built_from moves and the first build begins again, rewriting only what moved.
DROP INDEX change_postings_term_idx;
DROP INDEX change_docs_state_idx;
DROP INDEX change_docs_owner_idx;
DROP INDEX change_docs_repo_idx;
DROP INDEX change_docs_id_idx;
DROP TABLE change_postings;
DROP TABLE change_docs;
DROP TABLE change_index_state;

CREATE TABLE change_docs (
    doc       INTEGER NOT NULL PRIMARY KEY,
    id        BLOB    NOT NULL,
    repo_id   TEXT    NOT NULL,
    owner     TEXT    NOT NULL,
    state     TEXT    NOT NULL,
    text_hash BLOB    NOT NULL
);

CREATE UNIQUE INDEX change_docs_id_idx ON change_docs (id);       -- the walks find a change's document by the change's id
CREATE INDEX change_docs_repo_idx ON change_docs (repo_id, doc);   -- a search scoped to one repository reads its newest documents first; the purge follower's rotation and drop
CREATE INDEX change_docs_owner_idx ON change_docs (owner, doc);    -- a search scoped to one owner reads their newest documents first
CREATE INDEX change_docs_state_idx ON change_docs (state, doc);    -- a search scoped to one state reads its newest documents first

-- A slot is its document's key shifted 16 bits left plus the term's place among the document's terms.
-- No foreign key: this file enforces them, and the slot range is the only link a document has.
CREATE TABLE change_postings (
    slot INTEGER NOT NULL PRIMARY KEY,
    term TEXT    NOT NULL
);

CREATE INDEX change_postings_term_idx ON change_postings (term, slot);    -- a word's documents newest first, and whether one document holds it: slot is named since Turso seeks on no index's trailing row id

CREATE TABLE change_index_state (
    id          INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    built_from  TEXT    NOT NULL,
    through_pos INTEGER NOT NULL,
    seen_pos    INTEGER NOT NULL,
    back_pos    INTEGER NOT NULL,
    back_floor  TEXT    NOT NULL,
    updated_at  INTEGER NOT NULL
);

INSERT INTO change_index_state (id, built_from, through_pos, seen_pos, back_pos, back_floor, updated_at)
VALUES (1, '', 0, 0, -1, '', 0);
