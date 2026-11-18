-- A change is found by its words.
--
-- WHO HAS TO AGREE ON IT: this node alone: derived from replicated rows by this node's own walk, and rebuilt from them.
-- A document is numbered by this node; its postings are the slots seq << 16 up to (seq + 1) << 16, which an edit or a drop deletes as one range, so no term list is kept.
-- Measured (BenchmarkTheChangeIndexBuild, numbered a year in): about 2.0 KB a change of short text, 8.1 KB at 150
-- distinct words and 46 KB at a 32 KiB description; about 120 GB a year at 40 000 changes a day of 150 distinct words.
CREATE TABLE change_docs (
    seq       INTEGER NOT NULL PRIMARY KEY,
    id        BLOB    NOT NULL,
    repo_id   TEXT    NOT NULL,
    owner     TEXT    NOT NULL,
    state     TEXT    NOT NULL,
    text_hash BLOB    NOT NULL
);

CREATE UNIQUE INDEX change_docs_id_idx ON change_docs (id);       -- the walk finds a change's document by the change's id
CREATE INDEX change_docs_repo_idx ON change_docs (repo_id, seq);   -- a search scoped to one repository reads its newest documents first; the purge follower's rotation and drop
CREATE INDEX change_docs_owner_idx ON change_docs (owner, seq);    -- a search scoped to one owner reads their newest documents first
CREATE INDEX change_docs_state_idx ON change_docs (state, seq);    -- a search scoped to one state reads its newest documents first

-- A slot is its document's seq shifted 16 bits left plus the term's place among the document's terms.
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
    last_seq    INTEGER NOT NULL,
    rebuilding  INTEGER NOT NULL CHECK (rebuilding IN (0, 1)),
    updated_at  INTEGER NOT NULL
);

INSERT INTO change_index_state (id, built_from, through_pos, seen_pos, last_seq, rebuilding, updated_at)
VALUES (1, '', 0, 0, 0, 0, 0);
