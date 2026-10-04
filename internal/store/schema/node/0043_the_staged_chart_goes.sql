-- What this node kept about the org chart's log goes, and so does the scrub
-- stamp on a revision.
--
-- # The staged chart
--
-- 0040 kept a chart an offline `crewlet config import` left for this node to
-- publish on the chart's log at its next start, because no command-line
-- process opens a broker. The chart is the company document's again, so an
-- import stores the whole document and nothing is staged.
DROP TABLE chart_import_staged;

-- # The chart a revision ran
--
-- 0039 recorded, on a revision this node activated, the position on the
-- chart's log the company's organisation was read at. A revision's org is the
-- revision's own document now, so there is no second position to record.
ALTER TABLE company_config DROP COLUMN chart_position;

-- # The scrub stamp
--
-- 0038 stamped a superseded revision whose personal fields `crewlet config
-- scrub` had erased. The command is gone and nothing writes or reads the
-- stamp; a column no writer fills is a value every reader is entitled to
-- misread.
ALTER TABLE company_config DROP COLUMN scrubbed_at;

-- # The diverged log
--
-- 0031 remembers, per stream, a record this node consumed that the log no
-- longer holds at the same position. A row for the chart's log names a stream
-- no node registers, so it would never be cleared and would be reported for
-- ever.
DELETE FROM statelog_diverged WHERE stream = 'CREWLET_CHART_LOG';
