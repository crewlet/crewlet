-- Index the placement hint the seat sweep reads.
--
-- ``LeaseStore.preferred_resources`` runs once per sweep on every node —
-- every 5 seconds, per node — and its predicate is
-- ``resource LIKE 'seat:%' AND preferred = $node``.  Neither existing
-- index serves it: 019 indexes only ``owner`` and ``expires_at``, and
-- the ``resource`` primary-key btree cannot answer a LIKE prefix under a
-- non-C collation.  So the query was a sequential scan of the whole
-- table, and lease rows never shrink — ``release`` expires a row in
-- place rather than deleting it, so the epoch stays monotonic and the
-- hint survives.  The scan therefore grows with every seat the company
-- has ever had, not with the seats it has now.
--
-- Partial on a non-empty hint because that is the only thing the query
-- matches: an unhinted row can never satisfy ``preferred = $node``, and
-- most rows are unhinted.
CREATE INDEX IF NOT EXISTS leases_preferred_idx
    ON leases (preferred, resource)
    WHERE preferred <> '';
