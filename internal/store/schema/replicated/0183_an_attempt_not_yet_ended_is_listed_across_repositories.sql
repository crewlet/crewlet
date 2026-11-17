-- An attempt not yet ended is listed across repositories.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; code_checks stays reproducible, and an index adds no row.
-- About 130 bytes an attempt while it is queued or running and none once it ends: its queue writes the entry, its claim rewrites it (the state leads the key) and its ending removes it.
CREATE INDEX code_checks_unended_idx ON code_checks (state, finished_at, repo_id, commit_oid, target_ref, name, attempt)
    WHERE state IN ('queued', 'running');    -- every repository's attempts not yet ended, by state: a standing listing of every repository's checks (checkstanding.go)
