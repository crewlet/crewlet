-- A published layer is derived.
--
-- A git node serving a repository answers a fetch from a multi-pack-index and
-- its bitmap over the repository's packs; without them every request walks the
-- history it sends. Writing that index costs the history, whatever was added
-- since the last one, so the code host builds it ONCE for the fleet: the
-- repository's maintainer writes git's own incremental multi-pack-index layer —
-- its bitmap, and the commit-graph layer over the same packs — stores its bytes
-- in the object store, and names it on the code log, where every git node
-- reads it and installs it. A layer is published as a compaction record that
-- carries it (internal/code's LayerRef), decided by the repository's committer
-- like any compaction, so everything that moves a repository's packs or its
-- index has one writer.
--
-- code_layers is a repository's PUBLISHED CHAIN, root first: one row a layer,
-- `seq` 1.. in chain order and contiguous. The chain order is the only thing
-- that names a layer's base — a layer file does not — so the rows are its
-- authority. A publication replaces the chain's top rows it names and adds
-- one, so the table holds at most code.MaxPublishedLayers rows a repository,
-- replaced in place, and every read of a chain reads it whole by its key. No
-- index beside the key: a chain is 32 rows at most, and every reader — the
-- decide, the apply, a snapshot, an answer — reads all of them. `packs` is the
-- JSON array of the git checksums of every pack the layer covers, sorted: a
-- layer covers every pack live as of its plan that its base does not, so the
-- rows of one chain name disjoint packs.
--
-- code_layer_parts is the objects a layer's bytes are stored in — its
-- multi-pack-index, its bitmap and its commit-graph layer back to back, cut
-- into parts of code.LayerPartBytes, uploaded together — one row a part, `part`
-- 0.. in byte order. It is the table that NAMES OBJECTS, declared
-- objstore.Derived in internal/objstore/references (ADR-0074): any git node
-- can build the layer again from the packs code_packs names, so a part is kept
-- from the collector while a row names it, never audited, never copied,
-- counted or asked after by a backup (on nats its bytes ride the bucket's
-- stream snapshot uncounted), and copied by a store move. A publication that
-- replaces a layer removes its rows at once, with no grace: whatever reads a
-- layer it can no longer fetch reads the rows again for what replaced it.
--
-- The five code_repos columns are what the code host's publication due listing
-- needs without a sum over code_packs. published_packs and published_objects
-- are the live pack rows the chain covered, and the objects they held, as the
-- last publication RECOUNTED them from the rows: every row whose git checksum
-- a layer names, so two rows of one checksum count twice. Nothing else moves
-- them. A push raises live_packs and live_objects alone, so a pack pushed
-- since that recount counts in the tail (live_packs − published_packs) even
-- where it is byte-identical to a published one, until the next publication
-- recounts it: an active or archived repository's tail is the packs no layer
-- covers or more, never fewer. Keeping it exact would make every push's apply
-- read the chain's checksums, up to code.MaxPacksPerRepo of them, to correct a
-- count whose error costs a listing row and a plan that finds nothing to
-- publish (the index below). published_at is the last publication's broker
-- instant as store.EncodeTime writes it (unix microseconds), 0 for a
-- repository never published; publish_took_ms is how long that publication's
-- build took; and layers_position is the packed position of the last record
-- that changed code_layers — a snapshot's delta from below it serves the chain
-- whole. Each default is the value that says no layer was ever published,
-- which SQLite also requires of a NOT NULL column added to a table.
--
-- Class: code_layers and code_layer_parts are reproducible: written only by the
-- applier, from the compaction records that carry a layer and the purge that
-- removes a repository whole. code_repos stays reproducible: the five columns
-- are written only from those compaction records.
CREATE TABLE code_layers (
    repo_id        TEXT    NOT NULL,
    seq            INTEGER NOT NULL,
    layer_id       TEXT    NOT NULL,
    base_id        TEXT    NOT NULL,
    graph_id       TEXT    NOT NULL,
    packs          TEXT    NOT NULL,
    midx_bytes     INTEGER NOT NULL,
    bitmap_bytes   INTEGER NOT NULL,
    graph_bytes    INTEGER NOT NULL,
    objects        INTEGER NOT NULL,
    commits        INTEGER NOT NULL,
    git            TEXT    NOT NULL,
    took_ms        INTEGER NOT NULL,
    added_position INTEGER NOT NULL,
    PRIMARY KEY (repo_id, seq)
);

CREATE TABLE code_layer_parts (
    repo_id  TEXT    NOT NULL,
    layer_id TEXT    NOT NULL,
    part     INTEGER NOT NULL,
    object   TEXT    NOT NULL,
    hash     TEXT    NOT NULL,
    size     INTEGER NOT NULL,
    PRIMARY KEY (repo_id, layer_id, part)
);
CREATE INDEX code_layer_parts_object_idx ON code_layer_parts (object);    -- objstore.ReferenceTable statements (ObjectsAmong, ReferencesAfter): the collector and a store move

ALTER TABLE code_repos ADD COLUMN published_packs INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN published_objects INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN published_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN publish_took_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE code_repos ADD COLUMN layers_position INTEGER NOT NULL DEFAULT 0;

-- PARTIAL, ON THE PREDICATE THE LISTING REPEATS VERBATIM: a repository whose
-- every live pack its last publication's recount found covered, and none
-- added since, has nothing to publish, and a deleted or purging one is
-- nobody's to publish, so the listing never pays for either. A pack pushed
-- since the recount puts its repository in the index even where a layer
-- already names its checksum (published_packs above): listed with nothing to
-- publish — which only a plan over its packs can tell, answering it not due —
-- until the next publication recounts. (published_at, id) is its order —
-- least recently published first, a repository never published (0) before
-- every other — and its cursor the last row's pair. What it costs is a
-- publication's own row update, which moves the key, and the row update a
-- push already makes, which moves a repository into the index with the first
-- pack it adds since the recount.
CREATE INDEX code_repos_unpublished_idx ON code_repos (published_at, id) WHERE live_packs > published_packs AND status IN ('active', 'archived');    -- the publication due listing (Reader.Read publication_due): least recently published first
