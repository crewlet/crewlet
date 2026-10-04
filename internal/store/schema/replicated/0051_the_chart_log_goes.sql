-- The org chart's log goes, with every table its applier wrote.
--
-- 0031 and 0032 made the org chart the state log's fourth domain: one record
-- per change on `CREWLET_CHART_LOG`, applied into the `chart_*` tables below on
-- every node. The chart is the company document's again — `roles:` and
-- `units:` are stored in every Tier B revision and built into the running
-- organisation once per applied epoch — so no node registers the domain, no
-- applier writes these tables and nothing reads them.
--
-- # The domain's own tables
--
-- Dropped whole, their indexes with them. Nothing in them carries over: the
-- organisation a node runs is the company document of the epoch it applied,
-- and a company that kept its chart on the log is imported again from its
-- document.
DROP TABLE chart_manages;
DROP TABLE chart_leads;
DROP TABLE chart_seats;
DROP TABLE chart_units;
DROP TABLE chart_history;
DROP TABLE chart_import_ledger;
DROP TABLE chart_removed;
DROP TABLE chart_ops;
DROP TABLE chart_log_deferred_scope;
DROP TABLE chart_log_deferred;
DROP TABLE chart_evictions;
DROP TABLE chart_log_generations;

-- # The framework's rows about that log
--
-- The framework keys its checkpoint and its anchors on the stream, and its
-- ledger watermarks on the ledger table. A row naming a log no node registers
-- is worse than dead weight: a snapshot's manifest is read from the checkpoint
-- rows as domains, and a recipient refuses an artefact naming a domain it does
-- not register — so a node that kept the chart's checkpoint could donate to
-- nobody.
DELETE FROM statelog_cursor WHERE stream = 'CREWLET_CHART_LOG';
DELETE FROM statelog_anchor WHERE stream = 'CREWLET_CHART_LOG';
DELETE FROM statelog_ops_lost WHERE ops_table = 'chart_ops';
DELETE FROM statelog_ops_lost_kind WHERE ops_table = 'chart_ops';
