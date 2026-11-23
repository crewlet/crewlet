-- A check names no cell.
--
-- A check runs in one place: a container on the Gitlet node that claims it,
-- made from the company's providers.sandbox.local.image
-- (internal/gitlet/checkrun). Its definition names no cell, so
-- code_checks.run_in holds nothing a reader needs, and code_checks_queued_idx,
-- which 0131 led with it, is the queued attempts oldest queued first: a
-- runner's claim walk reads one range, and reads none while its node's
-- gitlet.max_checks containers are all running.
--
-- The index is dropped FIRST and created again under its own name: Turso
-- refuses `ALTER TABLE … DROP COLUMN` for a column an index names (0136), and
-- the claim walk is the one reader that seeks the index. 0103, 0113 AND 0131
-- ARE NOT EDITED: `schema_migrations` keys on the filename, so a store that
-- applied them would never see the change.
--
-- Class: code_checks stays REPRODUCIBLE (0113): written only by the applier,
-- from the records on a repository's checks subject, and removed only by
-- those records' horizon, by the code-retention duty's checks_sweep records on
-- the same subject, and by the repository's purge. An index carries no rows of
-- its own.
DROP INDEX code_checks_queued_idx;

CREATE INDEX code_checks_queued_idx ON code_checks (queued_at, repo_id, commit_oid, target_ref, name, attempt)
    WHERE state = 'queued';    -- the attempts waiting for a runner, oldest queued first: a runner's claim walk

ALTER TABLE code_checks DROP COLUMN run_in;
