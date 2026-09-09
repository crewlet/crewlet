-- The knowledge base's projection leaves this estate, and with it the last
-- projection there was.
--
-- 0020 created these seven tables as a REBUILDABLE local copy of the pages
-- family's coordination bucket: a projector followed the change feed and wrote
-- them, and a reader read them. `0005_the_pages_domain_lands.sql` in the
-- REPLICATED estate is what replaced them — the same rows, written by a
-- deterministic applier from an ordered stream, with the checkpoint committed
-- in the same transaction.
--
-- The move is not a relocation of a cache, for the reason 0024 gave when the
-- tracker made it: a projection is per-node state with a cursor that can be
-- behind and no way to say how far, where the applier's copy has a POSITION on
-- a log — so a read reports the level it answered at and a caller can wait for
-- its own write. Nothing here is copied across: a statement may not name a
-- table in both files, and every row these tables hold is derivable from the
-- log by replaying it, which is what a node with no pages rows does on its
-- first boot after this migration.
--
-- CHILDREN FIRST, for 0024's reason: every one of these carries `REFERENCES
-- pages(id) ON DELETE CASCADE`, and dropping the parent while a child still
-- references it leaves the child's foreign key pointing at a table that is
-- gone — the next write to it fails on a constraint naming a table nobody can
-- find, and the error names neither this migration nor the table that left.
DROP TABLE page_history;
DROP TABLE page_comments;
DROP TABLE page_revisions;
DROP TABLE page_watchers;
DROP TABLE page_labels;

-- The parent, and then the container table that was never a child.
DROP TABLE pages;
DROP TABLE page_containers;

-- ---------------------------------------------------------------------------
-- And the projection's own machinery, which has no families left to serve.
--
-- These two are not a family's tables — they are the PROJECTOR's, one row per
-- key and one cursor per family, and `internal/projection` is deleted in the
-- same change. The last family it served was this one.
--
-- What replaced the cursor is `statelog_cursor` in the replicated estate, and
-- the difference is the whole point of the move: this row carried a `hydrated`
-- boolean beside a revision, because a projector following a bucket's change
-- feed can say only whether a boot reconcile finished — where a checkpoint is
-- a place on an ordered log, and every read derived from it can say how far
-- behind it is rather than merely whether it has started.
--
-- What replaced `projection_keys` is nothing at all, and that is the honest
-- answer: it existed so a reconcile could tell "we applied this key's removal"
-- from "we never saw it" without re-fetching the bucket. An ordered log has no
-- such question — a record either is below this node's checkpoint or is not.
DROP TABLE projection_keys;
DROP TABLE projection_cursor;

-- WHAT STAYS, and why it is not a leftover: kb_docs, kb_postings, kb_vectors
-- and kb_vectors_bin are the SEARCH INDEX, which is this node's own derived
-- copy rather than a projection of a shared record. It is rebuildable, it is
-- deliberately outside every identity claim, and it now reads its sources
-- across the estate boundary — a batch from the replicated tables, a batch
-- from these — because no read joins the two.
