-- A page knows who links to it.
--
-- "Linked from" is the one question a knowledge base answers that a search
-- cannot: which runbooks point at this one, and which tasks cite it. A page is
-- addressed by its id (`#/knowledge/pages/<id>`, or `/pages/<id>`), and the
-- bodies that carry one are already read, tokenised and written down by the
-- lexical indexer — so the links are derived THERE, from exactly the text the
-- index holds, by the pure grammar `pages.Links` owns.
--
-- WHO HAS TO AGREE ON IT: this node alone. It is an index over rows that live
-- in the OTHER estate, with the lifecycle of `kb_docs` beside it: derived,
-- droppable, rebuilt by a local walk. A peer's copy would buy nothing, and the
-- rows it points at are the same on every node because the bodies are.
--
-- ONE ROW PER (source document, target page), keyed on the INDEX's own
-- document id rather than on the source's bare id: the index is one table over
-- the pages and the work items, and a page and a task can share an id-shaped
-- string, so `page:<id>` and `item:<id>` are the two sources a link can have.
-- It cascades from `kb_docs`, which is the whole of its removal — a page that
-- is trashed, a task that is removed and a document the orphan pass drops all
-- take their links with them in the one delete that already happens.
CREATE TABLE page_links (
    doc_id    TEXT NOT NULL REFERENCES kb_docs(id) ON DELETE CASCADE,
    target_id TEXT NOT NULL,
    PRIMARY KEY (doc_id, target_id)
);

-- "Who links to this page" is the only read, and it is by target.
CREATE INDEX page_links_target_idx ON page_links (target_id);

-- THE DERIVATION a document's row was built under, beside the source version
-- it was built from.
--
-- `source_rev` says whether the TEXT moved; this says whether what the indexer
-- derives from unchanged text moved — and this migration is the first time it
-- has: every row already indexed was written before any link was, and its
-- version still matches its source, so the walk would never look at it again
-- and no existing page would ever list a backlink. A row below the indexer's
-- current derivation is re-derived on the next lap exactly as a stale one is,
-- which is the backfill: no copy of anybody's text, no rebuild of the postings
-- the search is reading meanwhile, one ordinary lap.
ALTER TABLE kb_docs ADD COLUMN derivation INTEGER NOT NULL DEFAULT 0;
