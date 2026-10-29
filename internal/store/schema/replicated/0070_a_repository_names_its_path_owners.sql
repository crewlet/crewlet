-- A repository names its path owners.
--
-- A repository's settings gain its PATH OWNERS (code.RepoSettings.PathOwners,
-- written in code.repos[].path_owners or by a tool's settings change): units
-- and seats, each owning the paths of the repository's tree its globs match
-- (gitref.ParsePathGlob) — whom a change touching those paths asks for a
-- review. They grant nothing: who reads and pushes is the chart's and the
-- tier's, and no read, push or fetch reads this table.
--
-- ONE ROW AN OWNER, its globs packed into it, and not a row a glob. A
-- repository names at most code.MaxPathOwners (64) owners of at most
-- code.MaxPathOwnerPaths (32) globs each, so a row a glob is up to 2 048 rows a
-- repository — and every record that carries a repository's settings replaces
-- them whole, which at that bound is 4 096 rows deleted and written in one
-- record's apply, past the rows the code domain holds a record's apply to
-- (code.MaxRecordRows, the applier's statelog.ApplyTxRowBudget, 4 000), and
-- past what the final purge may delete beside a part's rows. A row an owner is
-- 64 rows at most, as a repository's protection rules are. Nothing reads one glob of a repository apart from the
-- rest: a glob is asked whether a change's paths match it, and every glob the
-- repository names is asked at once. So the globs are packed, as
-- code_pushes.introduced packs a push's commit ids: sorted, one a line — a glob
-- holds no control character, so no newline is a glob's.
--
-- KEYED BY (repo_id, owner_kind, owner): a repository's owners read by the
-- key's prefix in the one order its settings carry them in
-- (code.CanonicalPathOwners), and a unit and a seat sharing a token two owners,
-- as the settings name them — by kind, then reference.
--
-- Class: code_path_owners is reproducible (code.ReproducibleTables): written
-- only by the applier, from the records that carry a repository's settings —
-- its create, a settings change and a restore, each replacing them whole — and
-- removed by the repository's purge, beside its protection rules.
CREATE TABLE code_path_owners (
    repo_id    TEXT NOT NULL,
    owner_kind TEXT NOT NULL,   -- code.OwnerKind: unit or seat
    owner      TEXT NOT NULL,   -- a unit's key or a seat's handle
    paths      TEXT NOT NULL,   -- its globs, sorted, one a line
    PRIMARY KEY (repo_id, owner_kind, owner)
);
