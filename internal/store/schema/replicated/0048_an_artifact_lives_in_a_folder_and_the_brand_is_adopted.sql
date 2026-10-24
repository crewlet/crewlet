-- An artifact lives in one folder, and the company's brand is adopted.
--
-- Two additions to the artifacts domain (internal/artifactsdomain), written by
-- its one applier from records on the artifacts log, and nothing else:
--
--   - FOLDERS. The company's folder tree is ONE subject on the log (`folder`,
--     no id), because every folder write reads the whole tree (sibling names,
--     the ancestor chain, the depth stamp, the live count). Each artifact
--     carries one `folder_id`, '' for the top level. A folder GRANTS NOTHING
--     on what it holds: an artifact's access is still its owner and its own
--     shares (ADR-0034). A folder's own visibility is its top-level folder's
--     owner and shares, decided as a relation like an artifact's.
--   - THE ADOPTED BRAND. An administrator adopts one version of a brand kit
--     (an artifact of kind `brand`) on ONE subject (`brand`, no id). The
--     adoption SNAPSHOTS that version: the canonical `brand.json`, the hash of
--     the stylesheet generated from it, and every file the brand serves, so
--     the adopted brand survives the kit's retention, trash and purge and
--     changes only by another adoption.
--
-- WHO HAS TO AGREE ON IT: every data node, identically. Every table below is
-- in the domain's reproducible set (artifactsdomain.ReproducibleTables), so a
-- replay from zero rebuilds it byte for byte and an identity claim compares
-- it across the fleet. The rules 0043 states for every replicated table hold
-- here: no foreign key, no UNIQUE outside a primary key (sibling names are
-- unique by the writer's snapshot on the tree subject, through the folded
-- `name_key` the record carries), no COLLATE, instants are the broker's.

-- artifact_folders: one row per folder, a removed one kept as a tombstone so a
-- late retry of its create can never bring the id back.
CREATE TABLE artifact_folders (
    -- A name-based uuid over the creating operation (and the folder's place
    -- in a chain one record creates), so a retry names the folder its first
    -- attempt made.
    id              TEXT    NOT NULL PRIMARY KEY,
    -- The parent folder, '' for a top-level folder.
    parent          TEXT    NOT NULL DEFAULT '',
    -- The top-level folder this one sits under, its own id for a top-level
    -- folder: what decides who may see it, read without walking the chain.
    root            TEXT    NOT NULL,
    -- The name as written (NFC, trimmed), and the case-folded key the writer
    -- computed and the record carries. The applier compares keys and never
    -- folds, so a Unicode table change in a later Go cannot split nodes.
    name            TEXT    NOT NULL,
    name_key        TEXT    NOT NULL,
    -- The owner, a seat handle or a machine login, and its actor kind: the
    -- kind opens the lead arm exactly as an artifact's does.
    owner           TEXT    NOT NULL,
    owner_kind      TEXT    NOT NULL,
    -- The depth bound in force when the folder was created
    -- (artifacts.MaxFolderDepth), STAMPED so the applier enforces the bound
    -- the record was decided under rather than its own build's constant. A
    -- later build may only raise it.
    max_depth       INTEGER NOT NULL,
    -- live or removed.
    status          TEXT    NOT NULL,
    created_by      TEXT    NOT NULL,
    created_by_kind TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_by      TEXT    NOT NULL,
    updated_by_kind TEXT    NOT NULL,
    updated_at      INTEGER NOT NULL,
    removed_by      TEXT    NOT NULL DEFAULT '',
    removed_by_kind TEXT    NOT NULL DEFAULT '',
    removed_at      INTEGER NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL
);
CREATE INDEX artifact_folders_parent_idx ON artifact_folders (parent, status, name_key);   -- one folder's children, A to Z, and a sibling's name
CREATE INDEX artifact_folders_root_idx ON artifact_folders (root, status);                  -- one top-level folder's tree

-- artifact_folder_shares: one audience's role on one TOP-LEVEL folder, on the
-- artifact share ladder. A subfolder holds no share of its own: its top-level
-- folder decides it, so every ancestor of a folder a reader sees is one they
-- see too.
CREATE TABLE artifact_folder_shares (
    folder_id       TEXT    NOT NULL,
    -- seat, unit or company; the id is the seat handle, the unit key, or
    -- empty for the company.
    audience_kind   TEXT    NOT NULL,
    audience_id     TEXT    NOT NULL,
    -- viewer, contributor or editor.
    role            TEXT    NOT NULL,
    granted_by      TEXT    NOT NULL,
    granted_by_kind TEXT    NOT NULL,
    granted_at      INTEGER NOT NULL,
    PRIMARY KEY (folder_id, audience_kind, audience_id)
);
CREATE INDEX artifact_folder_shares_audience_idx ON artifact_folder_shares (audience_kind, audience_id, folder_id); -- what one seat, unit or the company sees

-- artifact_folder_history: what happened to the tree, one row per record,
-- refusals included, so a folder writer reads its own verdict back. The
-- summary names folder ids and counts, never a name or an artifact title.
-- Kept artifacts.HistoryRetention, pruned by the applier in bounded batches.
CREATE TABLE artifact_folder_history (
    folder_id   TEXT    NOT NULL,
    position    INTEGER NOT NULL,
    op_id       TEXT    NOT NULL,
    op          TEXT    NOT NULL,
    outcome     TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    actor       TEXT    NOT NULL,
    actor_kind  TEXT    NOT NULL,
    operator_id TEXT    NOT NULL DEFAULT '',
    elevated    INTEGER NOT NULL DEFAULT 0,
    origin      TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL,
    summary     TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (folder_id, position)
);
CREATE INDEX artifact_folder_history_op_idx ON artifact_folder_history (op_id);                     -- a writer reading its own verdict back
CREATE INDEX artifact_folder_history_at_idx ON artifact_folder_history (at, folder_id, position);   -- the retention prune, in a total order

-- An artifact's folder, '' for the top level. Every artifact created before
-- this migration is at the top level, and a replay from zero of a create that
-- names no folder writes '' too, byte for byte.
ALTER TABLE artifacts ADD COLUMN folder_id TEXT NOT NULL DEFAULT '';
CREATE INDEX artifacts_folder_idx ON artifacts (folder_id, status, updated_at);             -- one folder's artifacts, newest first, and a removal's re-parent

-- The adopted brand stylesheet a framed version (html, design) was published
-- under: the generated CSS's SHA-256 in hex, '' for none (no brand adopted, or
-- the page opted out). The artifact origin places that stylesheet when it
-- serves the version, so a version keeps the brand it was approved with.
--
-- A PLAIN INDEX, not a partial one: the brand files' retention asks
-- `WHERE brand = ?` with a bound value, which a partial index cannot serve
-- (0045 states the same rule).
ALTER TABLE artifact_versions ADD COLUMN brand TEXT NOT NULL DEFAULT '';
CREATE INDEX artifact_versions_brand_idx ON artifact_versions (brand);                      -- whether any retained version still names an adoption's stylesheet

-- artifact_brand: the company's adopted brand. At most one row, key 'company';
-- a clear deletes it.
CREATE TABLE artifact_brand (
    key             TEXT    NOT NULL PRIMARY KEY,
    -- The kit the adoption was taken from and its version: provenance, and
    -- what "a newer version than the adopted one" is measured against. The
    -- kit may since have been trashed or purged; the snapshot below is what
    -- is served.
    kit_id          TEXT    NOT NULL,
    kit_version     INTEGER NOT NULL,
    -- The canonical brand.json of that version (internal/artifacts/brandformat),
    -- at most brandformat.MaxIndexBytes.
    brand_json      BLOB    NOT NULL,
    -- The SHA-256, in hex, of the stylesheet generated from it at adoption.
    -- Generated once and stored as an object, never regenerated at serve
    -- time, so the bytes behind a hash a version pins never change.
    css_sha         TEXT    NOT NULL,
    adopted_by      TEXT    NOT NULL,
    adopted_by_kind TEXT    NOT NULL,
    operator_id     TEXT    NOT NULL DEFAULT '',
    adopted_at      INTEGER NOT NULL,
    -- The packed position of the adoption record.
    position        INTEGER NOT NULL,
    version         INTEGER NOT NULL
);

-- artifact_brand_files: THE BRAND'S REFERENCE TABLE, declared in
-- internal/objstore/references so the collector never deletes a file an
-- adoption serves. One row per file an adoption's snapshot serves (the
-- generated stylesheet, each logo, each face), keyed by the adoption's
-- stylesheet hash so two adoptions that share a logo each keep their own row.
--
-- The public brand route on the artifact listener serves a file only if its
-- hash is here (an allowlist the applier writes): the file's SHA-256 in hex is
-- the path it is served at (`/_crewlet/brand/<hash>.<ext>`). Rows of an adoption that
-- is no longer current are pruned by the applier once no retained version
-- names its stylesheet (`artifact_versions.brand`).
CREATE TABLE artifact_brand_files (
    -- The adoption this file belongs to: its stylesheet's SHA-256 in hex.
    css_sha      TEXT    NOT NULL,
    -- The slot the file fills (`css`, `logo.primary`, `logo.on_dark`,
    -- `logo.mark`, `font.heading.<n>`, `font.body.<n>`).
    slot         TEXT    NOT NULL,
    content_type TEXT    NOT NULL,
    object       TEXT    NOT NULL,
    hash         TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    -- The packed position of the adoption that wrote the row.
    position     INTEGER NOT NULL,
    PRIMARY KEY (css_sha, slot)
);
CREATE INDEX artifact_brand_files_hash_idx ON artifact_brand_files (hash);                  -- the public route's allowlist
CREATE INDEX artifact_brand_files_object_idx ON artifact_brand_files (object);              -- the collector, the audit and the backup

-- artifact_brand_history: every adoption and clear, one row per record,
-- refusals included, so the adopting writer reads its own verdict back and an
-- administrator can see who changed the company's brand and when. Kept
-- artifacts.HistoryRetention, pruned by the applier in bounded batches.
CREATE TABLE artifact_brand_history (
    position    INTEGER NOT NULL PRIMARY KEY,
    op_id       TEXT    NOT NULL,
    op          TEXT    NOT NULL,
    outcome     TEXT    NOT NULL,
    reason      TEXT    NOT NULL DEFAULT '',
    kit_id      TEXT    NOT NULL DEFAULT '',
    kit_version INTEGER NOT NULL DEFAULT 0,
    actor       TEXT    NOT NULL,
    actor_kind  TEXT    NOT NULL,
    operator_id TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL,
    summary     TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX artifact_brand_history_op_idx ON artifact_brand_history (op_id);               -- a writer reading its own verdict back
CREATE INDEX artifact_brand_history_at_idx ON artifact_brand_history (at, position);        -- the retention prune, in a total order
