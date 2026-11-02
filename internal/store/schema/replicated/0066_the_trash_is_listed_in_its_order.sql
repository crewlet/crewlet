-- The trash reaper reads the trash in its listing's own order.
--
-- # pages_heads_trashed_idx
--
-- The trash reaper's listing (`trashedBeforeStatement` in
-- internal/doclet/trash.go) asks for the pages in the trash since before the
-- trash term's cutoff, OLDEST FIRST AND THEN BY ID, at most a pass's cap of
-- them: `WHERE trashed_at IS NOT NULL AND trashed_at < ? AND status = ? ORDER
-- BY trashed_at, id LIMIT ?`. The index 0005 declared for it was on
-- `trashed_at` alone, and what rides an index entry beside its own columns is
-- the row id, not the page's id, so measured on the pages plan fixture the
-- listing sought the index up to the cutoff and then sorted: every page
-- trashed before the cutoff read and sorted before the first could be
-- returned, on the pass that runs when a trash has piled up past its term, and
-- a pass stopped at its cap paid for all of them to return the cap.
--
-- (trashed_at, id) is the order itself, so the listing seeks the index up to
-- the cutoff in that order and its LIMIT stops it at the cap. Still partial on
-- `trashed_at IS NOT NULL`, which the statement restates so the planner can
-- choose it, so it holds the trashed pages and nothing else; the status the
-- listing also asks is read from each row it returns.
--
-- 0005 IS NOT EDITED: `schema_migrations` keys on the filename, so a database
-- that applied it would never see the change.
DROP INDEX pages_heads_trashed_idx;
CREATE INDEX pages_heads_trashed_idx ON pages_heads (trashed_at, id)
    WHERE trashed_at IS NOT NULL;                                               -- the trash reaper's listing, in its order
