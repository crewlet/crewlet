-- A clean expunge scan leaves the weekly listing for a week, on every node.
--
-- 0092 keyed the expunge listing's weekly arm on when a repository was last
-- EXPUNGED, so a repository its maintainer scanned and found not due stayed
-- listed: every git node read it through the estate every minute, and the
-- verdict lived in the scanning node's memory alone, so a restart, a rolling
-- upgrade or a git node joining or leaving had every repository it maintained
-- downloaded and walked again. A clean scan is now a record of its own
-- (gitlet.ExpungeScan, op expunge_scan), and the weekly arm keys on when the
-- repository was last JUDGED: expunge_scanned_at, the broker instant of its
-- last clean scan's record or of its last expunge's commit, whichever is
-- later — both write it.
--
-- 0092 IS NOT EDITED: `schema_migrations` keys on the filename. Its index is
-- dropped and made again over the new column. The column starts at 0, never
-- judged: a repository an earlier build expunged is scanned once more.
--
-- Class: code_repos stays reproducible: the column is written only by the
-- applier, from an expunge's commit and a scan's record.
ALTER TABLE code_repos ADD COLUMN expunge_scanned_at INTEGER NOT NULL DEFAULT 0;

DROP INDEX code_repos_expunge_due_idx;

-- PARTIAL, as 0092's was, on the repositories an expunge may be due in: least
-- recently judged first — a repository never judged (0) before every other —
-- and its cursor the last row's pair. A repository's entry moves with a
-- commit's or a clean scan's record — at most one a repository a week — and
-- with its status.
CREATE INDEX code_repos_expunge_due_idx ON code_repos (expunge_scanned_at, id) WHERE status IN ('active', 'archived');    -- the weekly expunge's listing: least recently judged first
