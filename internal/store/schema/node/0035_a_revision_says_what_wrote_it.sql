-- A config revision records WHAT wrote it, beside the label it was written
-- under.
--
-- created_by is a label — a token's name, a login, a node id, "reconcile
-- loop" — and a label cannot say whether a person or the engine wrote the
-- revision: the name spaces overlap, and nothing stops an operator token
-- being called `node`. The audit screen answered that by assuming, and drew
-- every revision as an operator's. The writer knows, so it states it
-- (store.AuthorKind): `operator` or `node`.
--
-- WHO HAS TO AGREE ON IT: this node alone, like every other column of this
-- table — a revision row is this node's copy. What makes the copies agree
-- is that the fleet's activation pointer carries the origin's author and
-- kind, and a peer adopting the revision stores what it carries instead of
-- the placeholder `peer` it used to write.
--
-- An empty string, never NULL, for a kind nobody recorded: a revision
-- adopted from a pointer an older build published says nothing about its
-- author, and '' is how a reader tells "not recorded" from a kind.
ALTER TABLE company_config ADD COLUMN created_by_kind TEXT NOT NULL DEFAULT '';

-- THE ROWS AN UPGRADE ALREADY HOLDS, classified by the only writers that
-- existed. Every label below is a literal an earlier build wrote, never a
-- guess from a name a person chose:
--
--   * source `fleet` is an ADOPTION, and the build that wrote it recorded
--     `peer` rather than the author. Nothing on this node knows who wrote
--     it, so the kind stays '' and the placeholder is cleared — `peer` was
--     never anybody's name, and showing it would be a claim. Configs.Adopt
--     fills both in the next time the fleet points at the revision.
--   * `node` is the boot seed's literal and `reconcile loop` the engine's
--     own writes through the config surface: the engine.
--   * everything else was written through /config, /setup or the CLI, all
--     of which record the credential or login that made the write.
UPDATE company_config SET created_by = '' WHERE source = 'fleet' AND created_by = 'peer';

UPDATE company_config
   SET created_by_kind = CASE
         WHEN source = 'fleet'                          THEN ''
         WHEN created_by IN ('node', 'reconcile loop')  THEN 'node'
         ELSE 'operator'
       END;
