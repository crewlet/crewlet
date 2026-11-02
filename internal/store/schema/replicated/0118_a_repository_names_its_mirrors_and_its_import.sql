-- A repository names its mirrors and its import.
--
-- A repository may follow, publish to, or be filled from a repository at
-- another host (internal/code's mirror.go):
--
--   - mirror_from makes it a PULL MIRROR: it is imported from its source when
--     it is created, then moves only by the syncs its home makes from it, and
--     takes no push and no change of its own (refused repo_mirrored);
--   - mirror_to makes it a PUSH MIRROR: its home pushes the refs it names to
--     a repository at another host, as one seat's own identity there;
--   - import_source is what its create filled it from — the create's own
--     import, or a pull mirror's source — fixed at the create, so a mirror_from
--     a settings change moves meanwhile is the next sync's to follow.
--
-- Each is the JSON of code.MirrorSource, code.MirrorTarget and
-- code.ImportSource, and '' for none — the value that says there is no
-- mirror, and what SQLite requires of a NOT NULL column added to a table. A
-- credential in one is a ${VAR} REFERENCE, never a token: the domain refuses
-- any other, so no secret rests in a row.
--
-- status takes `importing` beside the others, with no DDL: a create that
-- imports makes its repository importing, and it takes no write until its
-- import is done — the listings show it, while the compactor's, the
-- publisher's and the expunge's listings, each partial on active and archived,
-- pass it by. A restore brings a repository whose import is not done back
-- importing, by import_source and imported_at.
--
-- The import's progress and a pull mirror's last sync are the records' that
-- carry them, as the home's importer lands them: import_seq is how many of
-- an import's chunks have landed (0 for none), imported_at the broker instant
-- of the record that ended it as store.EncodeTime writes it (0 while it runs,
-- and for a repository that imported nothing), import_error why it ended
-- short ('' for none), and mirror_synced_at the instant of a pull mirror's
-- last sync (0 for never).
--
-- Class: code_repos stays reproducible: every column is written only by the
-- applier, from the records that carry them.
ALTER TABLE code_repos ADD COLUMN mirror_from TEXT NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN mirror_to TEXT NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN import_source TEXT NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN import_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN imported_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN import_error TEXT NOT NULL DEFAULT '';
ALTER TABLE code_repos ADD COLUMN mirror_synced_at INTEGER NOT NULL DEFAULT 0;

-- PARTIAL, ON THE PREDICATE ITS LISTINGS REPEAT VERBATIM: the repositories
-- being imported, the pull mirrors among the active ones least recently synced
-- first, and the push mirrors — each a few of a company's repositories, and
-- each listing reading nothing else. What each costs is the records that set or
-- clear a mirror, start or end an import and land a sync; no push touches them.
CREATE INDEX code_repos_importing_idx ON code_repos (id) WHERE status = 'importing';    -- imports_due: the repositories being imported, by id
CREATE INDEX code_repos_mirrored_idx ON code_repos (mirror_synced_at, id) WHERE mirror_from != '' AND status = 'active';    -- mirrors_due: the pull mirrors least recently synced first
CREATE INDEX code_repos_mirror_to_idx ON code_repos (id) WHERE mirror_to != '';    -- mirrors_due: the push mirrors, by id
