-- The lexical index gains the same SHARD the vector tables did, for the same
-- reason and computed by the same function.
--
-- See `0006_the_corpus_gets_shards.sql` in the replicated estate: a bucket is a
-- stable hash of a document's own identity, unrelated to every other thing
-- this engine partitions on, and a query still consults every bucket. What it
-- buys is a scan whose cost is measurable per bucket range.
--
-- THE ROWS GO HERE TOO, and here it costs nothing at all: kb_docs and its
-- postings are derived from the source rows with no network, and the indexer
-- rebuilds them on its own sweep. `kb_postings` has no shard of its own — a
-- posting belongs to its document, so the predicate is on the join rather than
-- on the term.
DROP INDEX kb_docs_source_idx;
DROP INDEX kb_docs_container_idx;
DROP INDEX kb_postings_doc_idx;
DROP TABLE kb_postings;
DROP TABLE kb_docs;

CREATE TABLE kb_docs (
    id          TEXT    NOT NULL PRIMARY KEY,
    source      TEXT    NOT NULL,
    source_id   TEXT    NOT NULL,
    -- The bucket, computed once by the indexer from (source, source_id) — the
    -- same value the vector tables hold for the same document, because it is
    -- the same function over the same two strings.
    search_shard INTEGER NOT NULL,
    container   TEXT    NOT NULL DEFAULT '',
    title       TEXT    NOT NULL DEFAULT '',
    excerpt     TEXT    NOT NULL DEFAULT '',
    length      INTEGER NOT NULL DEFAULT 0,
    source_rev  INTEGER NOT NULL DEFAULT 0,
    indexed_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX kb_docs_source_idx ON kb_docs (source, source_id);

-- AND kb_docs_container_idx DOES NOT COME BACK, which is a fix rather than an
-- omission: with it, a scoped search drove on kb_docs and probed the postings
-- per document instead of driving on the term. A company has a handful of
-- containers, so `container = ?` selects a large fraction of the corpus, where
-- a term appears in a small one. Measured on a 1 000-document fixture the
-- planner took it every time a search named a container -- which is every
-- knowledge prefetch a seat makes.
--
-- THE SHARD GETS NO INDEX EITHER, for the same reason and one more. A bucket
-- range is seekable, so an index over it is the same trap with a wider door:
-- the planner would drive on one sixty-fourth of the corpus rather than on the
-- term. And it would not pay for itself anywhere else -- nothing walks a
-- bucket range; the shard is only ever a filter on a row the join already
-- reached. This is the same conclusion `0004_the_vector_indexes_go.sql`
-- reached in the other estate, measured there at 1 min 44 s against 50 ms.

CREATE TABLE kb_postings (
    term   TEXT    NOT NULL,
    doc_id TEXT    NOT NULL REFERENCES kb_docs(id) ON DELETE CASCADE,
    freq   INTEGER NOT NULL,
    PRIMARY KEY (term, doc_id)
);

CREATE INDEX kb_postings_doc_idx ON kb_postings (doc_id);
