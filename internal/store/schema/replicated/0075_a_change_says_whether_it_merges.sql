-- A change says whether it merges.
--
-- Whether a change's head revision merges cleanly into the tip of the branch
-- it targets is a function of two commits alone, which the repository's HOME
-- git node merges in a scratch of its own (internal/code/home's
-- `git merge-tree --write-tree`) and records on the change: a `mergeability`
-- record on the change's own subject (0072), which the repository's
-- committing data node publishes for it, as it publishes a revision's facts
-- (0074). Every reader of it after that reads this row, never git: a change's
-- page, its readiness.
--
-- code_mergeability is one row a change, keyed by the change: its LATEST
-- merge, replaced whole by every later record the decide takes.
--   - revision is the revision merged; target_ref, target_oid and
--     target_position the target branch, the tip the head was merged into and
--     the packed position of the record that moved the branch there. For one
--     revision against one branch the row moves only forward: a record of an
--     earlier tip is refused, and so is the same tip again once decided.
--   - state is what the merge did: `clean`, `conflict`, or `unknown` — a
--     merge its home could not decide within its deadline, read again at the
--     home's refresh as a moved target is.
--   - conflicts_count and conflicts are the conflicted paths the record
--     names, at most code.MaxConflictPaths and code.MaxConflictBytes as JSON,
--     NUL-separated (no path of a tree holds a NUL); conflicts_omitted is how
--     many more the merge left conflicted.
--   - node is the git node that merged: the repository's home as it took
--     itself to be.
--   - bytes is what the wide column weighs, so a purge part's walk weighs it
--     by the change's conversation's text_bytes, which counts it, and reads no
--     wide row it keeps.
--   - position is the record that wrote the row, and at its broker instant —
--     what the home's recheck of a moved target, or of an undecided merge, is
--     paced by: due again once the row is code/home's MergeabilityRefresh old
--     (code.ReadChangesDue).
--
-- THE WIDE COLUMN IS DECLARED LAST: a list of paths does not always fit a leaf
-- page, and a row read for a column before it stops there (0066's
-- measurement).
--
-- NO repo_id and NO INDEX beyond the key, as a revision's facts have none: the
-- home's due listing reaches a change's row through the change, one seek of
-- its key a live change — the repositories' listing, a repository's due
-- changes and its due rechecks alike — and so does a change's read and a
-- purge.
--
-- Class: code_mergeability is reproducible: written only by the applier, from
-- the records on a change's own subject, and removed only by its repository's
-- purge, a change's mergeability before its row.
CREATE TABLE code_mergeability (
    change_id         TEXT    NOT NULL PRIMARY KEY,
    revision          INTEGER NOT NULL,
    target_ref        TEXT    NOT NULL,
    target_oid        TEXT    NOT NULL,
    target_position   INTEGER NOT NULL,
    state             TEXT    NOT NULL,
    conflicts_count   INTEGER NOT NULL DEFAULT 0,
    conflicts_omitted INTEGER NOT NULL DEFAULT 0,
    node              TEXT    NOT NULL,
    bytes             INTEGER NOT NULL DEFAULT 0,
    position          INTEGER NOT NULL,
    at                INTEGER NOT NULL,
    conflicts         TEXT    NOT NULL DEFAULT ''
);
