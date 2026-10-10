-- A page's readers are indexed in the order they are read in.
--
-- 0023 indexed usage_reads on (page_id, day) for "who has read this page
-- lately", and the read is in (page, day, node, seat, via) order — the order a
-- page of it is cut and resumed in (internal/usage, PageReadsPage), so that one
-- seat's day of reads of one page is never split across two pages. An index on
-- the first two columns of that order seeks the page and its days and then
-- SORTS every row it found before handing back the first, so a statement that
-- stops where a page ends had already read, and sorted, the page's every read
-- in the window — and a read walked in N pages paid that N times over.
--
-- Indexed on the whole order, a statement seeks the page id and the day the
-- page before ended on and streams from there in index order; the page that
-- stops it has read its own rows and nothing past them. The index REPLACES the
-- narrow one rather than sitting beside it: every statement that used the
-- narrow one seeks the same two leading columns of this one, and two indexes
-- on one table cost every applier write a second entry for the same seek.
-- TestEveryPagedStatementSeeksItsKeyAndStreams holds the plan.
--
-- Class: usage_reads stays DIVERGENT; an index carries no rows of its own.
DROP INDEX usage_reads_page_idx;

CREATE INDEX usage_reads_page_idx ON usage_reads (page_id, day, node, agent_id, via);
