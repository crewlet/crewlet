-- A page comment says whether it asks, and whether somebody has answered it.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_PAGES_LOG stream by its one applier, as the rest of pages_comments.
--
-- `asks` is an agent's statement that its comment asks (ADR-0113), and
-- `answered` that a later comment by a seat it named, or by anybody when it
-- named nobody, answered it. A reply wakes the authors of the open asks in its
-- thread, which the partial index below reads without touching any other row.
ALTER TABLE pages_comments ADD COLUMN asks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pages_comments ADD COLUMN answered INTEGER NOT NULL DEFAULT 0;
CREATE INDEX pages_comments_open_asks_idx ON pages_comments (page_id, version)
    WHERE asks = 1 AND answered = 0;                                              -- a page's open asks
