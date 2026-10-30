-- A page listing is read in its own order.
--
-- Every listing of the knowledge base (`Reader.List` in internal/doclet/read.go)
-- is ordered by (container, title, id), and no index held that order:
-- `pages_heads_container_idx` was (container, status, title), with the status
-- between the two columns the order needs and no id, and
-- `pages_heads_parent_idx` was the parent alone. Measured on the pages plan
-- fixture, every window of every listing — the workspace, one container, a
-- tree level, a person's watched pages — read every page the listing matched
-- and sorted them all to return fifty, and a later window resumed through a
-- nested disjunction no index could seek, so paging a knowledge base of N
-- pages read N pages per window.
--
-- # pages_heads_container_idx
--
-- (container, title, id) is the order itself, so a listing walks it from the
-- first row its window needs and its LIMIT stops the walk. The status rides
-- after the order rather than inside it: no listing names one status (a tree
-- shows the published and the drafts together), so a status between the
-- container and the title would split every container's order in two, while
-- after it the per-container count of the knowledge base's rail
-- (`Reader.pageCounts`) still reads the index alone.
--
-- # pages_heads_parent_idx
--
-- (parent_id, container, title, id) serves a tree level the same way: one
-- page's children, and the top of a container — its pages with no parent,
-- `parent_id = '' AND container = ?` — are each an equality prefix of it
-- followed by the listing's order. A child count, and the read that re-files
-- a purged page's children under its parent (`reparentChildren`), seek its
-- leading column as they sought the old one.
--
-- # pages_heads_address_idx
--
-- Resolving a link is an equality on BOTH of its columns (`container = ? AND
-- title_norm = ?`), which either order of them seeks. Led by the container it
-- was a second index on the column the listing index leads with, and the
-- planner, which has no statistics to tell them apart, took it for a read
-- bounded on the container alone — one container's listing, a later window's
-- `container > ?` — and sorted everything it found, since its second column
-- is not the listing's. Led by the normalised title, it serves the one lookup
-- it exists for and nothing that bounds only a container can reach it.
DROP INDEX pages_heads_container_idx;
CREATE INDEX pages_heads_container_idx ON pages_heads (container, title, id, status);     -- a space's own listing, in its order
DROP INDEX pages_heads_parent_idx;
CREATE INDEX pages_heads_parent_idx ON pages_heads (parent_id, container, title, id);    -- one page's children, and a container's top level, in the listing's order
DROP INDEX pages_heads_address_idx;
CREATE INDEX pages_heads_address_idx ON pages_heads (title_norm, container);               -- resolve a link by title
