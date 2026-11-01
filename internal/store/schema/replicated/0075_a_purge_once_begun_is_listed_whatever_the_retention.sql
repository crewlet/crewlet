-- A purge once begun is listed whatever the retention.
--
-- 0074 indexed code_repos on (deleted_at, id) for every repository deleted OR
-- purging, and the code-retention pass listed both from it by one range: the
-- deletion instant at or before now less the CURRENT retention. A purging
-- repository is not saved by a longer retention — its rows are already half
-- gone, its name answers ErrPurging and a purging repository has no restore —
-- so a revision that lengthened code.deleted_retention_days mid-purge, or the
-- duty moving to a node on an epoch with a longer one, dropped it out of the
-- range and left it purging, holding its name, until the new retention passed
-- — up to a year. The pass now lists every PURGING repository first, whatever
-- the retention (PurgeStep itself never asks a purging repository's age), and
-- then the DELETED ones past it.
--
-- TWO PARTIAL INDEXES, ONE PER STATUS, on the predicate each statement repeats
-- verbatim, so each statement reads only the rows it lists. Found through the
-- one index on deleted_at, the purging rows past the cutoff are interleaved
-- with every repository deleted inside the retention — a statement looking for
-- the few purging ones reads every one of those, at every pass. A row moves
-- from one index to the other once, at its first purge part.
--
-- Class: code_repos is unchanged; an index carries no rows of its own.
DROP INDEX code_repos_deleted_idx;

CREATE INDEX code_repos_deleted_idx ON code_repos (deleted_at, id) WHERE status = 'deleted';    -- the code-retention pass's listing: the deleted past the retention, oldest first
CREATE INDEX code_repos_purging_idx ON code_repos (deleted_at, id) WHERE status = 'purging';    -- the code-retention pass's listing: every purge begun, whatever the retention
