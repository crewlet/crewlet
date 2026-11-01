-- A runner seeks the queue by the cell it has room in.
--
-- code_checks_queued_idx was the queued attempts oldest first, every cell's
-- in one range, and a git node's claim walk paged it from its head until the
-- node had no room left. A node whose room could not be spent on what it read
-- read on: the walk stopped only once every cell was full, so a node with an
-- e2b backend in its catalogue — configured for its seats, in a company whose
-- checks run in containers — paged the whole container backlog at every walk
-- with its container slots full, claiming nothing. A walk a second on each git
-- node, a page of definitions a read, and on a git node holding no estate each
-- read an estate-router call: the fleet's read load grew with its git nodes
-- times its queue.
--
-- So the index leads with the cell an attempt's definition names (run_in, ""
-- for none: the company's default cell, resolved at each walk against the
-- epoch it runs under, since an apply may move it): a walk reads, oldest
-- first, only the attempts of the cells this node has room in now, one seek
-- each, and one cell's backlog never costs a walk in another. The oldest-first
-- order a fleet's runners take the queue in is kept within a cell — the
-- fairness among attempts competing for the same slots — and the walk merges
-- the two ranges a cell can have (its own name and the default naming it).
--
-- The index is dropped and created again under its own name rather than
-- given a second one: it is the same reader's (the claim walk's) and nothing
-- else seeks it. replicated 0085 IS NOT EDITED: `schema_migrations` keys on
-- the filename, so a store that applied it would never see the change.
--
-- Class: code_checks stays reproducible (code.ReproducibleTables): an index
-- carries no rows of its own.
DROP INDEX code_checks_queued_idx;

CREATE INDEX code_checks_queued_idx ON code_checks (run_in, queued_at, repo_id, commit_oid, target_ref, name, attempt)
    WHERE state = 'queued';    -- the attempts waiting for a runner, by the cell they name, oldest queued first: a runner's claim walk
