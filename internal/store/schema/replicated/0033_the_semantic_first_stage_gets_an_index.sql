-- The semantic first stage gets an INDEX — an inverted file over the sign
-- codes kb_vectors_bin already holds. ADR-0028 is the decision; internal/search
-- (ivf.go) is the arithmetic, and this file is its storage.
--
-- # What this reverses, and only this
--
-- 0004 deleted every index on kb_vectors_bin, and was right to: the first stage
-- read EVERY row of the narrow table, and an index over a table whose rows are
-- all read is a second copy of the row order plus a random access per row. An
-- inverted file changes the premise rather than the finding — a probe reads the
-- rows of a few LISTS, a small share of the table — so what it needs is exactly
-- one thing 0004 never had to provide: a probed list as a CONTIGUOUS range.
--
-- # Why the index is COVERING, measured
--
-- A ONE-OFF comparison of the three shapes a list could be stored in, at
-- 40 000 fixture sources and 3 072 dimensions, reading the rows of a quarter
-- of the lists (warm, one reader, p50 — a harness written to choose the shape
-- and not committed, because only the shape it chose is worth re-measuring):
--
--   through a covering index            20 ms   (1.75 µs a probed row)
--   through (ivf_gen, ivf_list) alone   29 ms   (random access per row)
--   a rowid-clustered copy of the table 22 ms
--   the full scan it replaces           51 ms   (1.28 µs a row)
--
-- The clustered copy measured the same as the covering index and needs the
-- row's physical key rewritten on every re-filing; the plain index pays the
-- random access 0004 measured. The shape chosen is what the committed
-- BenchmarkSemanticIVFUnderLoad measures, at p95 and under load, with the
-- probe count a training actually installs: at 40 000 topical sources, half
-- the lists, 73 ms against the full scan's 116 with one reader and 219
-- against 294 with eight. The covering index is a second copy of the
-- narrow table — ≈ 450 bytes a source, ≈ 3 % of what kb_vectors itself holds —
-- and that is the price of a list being one sequential read.
--
-- # Why the embedding space LEADS the key
--
-- A probe is `model = ? AND dim = ? AND ivf_gen = ? AND ivf_list = ?`, and the
-- rows a rollout has not re-filed yet are `model = ? AND dim = ? AND ivf_gen <
-- ?`: both are one range only if the space comes first. Keyed on the list
-- alone, the not-yet-re-filed range would walk every other space's rows too —
-- during a model change, half the table.
--
-- # The columns, and why the rows are NOT rewritten here
--
-- `ivf_gen` is the generation of the index a row is filed under — the packed
-- position of the centroids record that installed it — and `ivf_list` its list
-- there. Zero is "filed under no index", which is what every existing row is:
-- the embedding duty trains the first index on its next tick and its reassign
-- records file every row, on every node, at the same position. A migration
-- that filed them here would need the centroids, which do not exist yet.
--
-- Class `Derived`, like the rest of the row: a function of the row's own code
-- and of kb_ivf, both in this file.
ALTER TABLE kb_vectors_bin ADD COLUMN ivf_gen  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE kb_vectors_bin ADD COLUMN ivf_list INTEGER NOT NULL DEFAULT 0;

CREATE INDEX kb_vectors_bin_ivf_idx ON kb_vectors_bin
    (model, dim, ivf_gen, ivf_list, source, source_id, container, search_shard, bits);

-- kb_ivf — the partition's index as the vector applier last installed it: ONE
-- row, because the log carries one centroids subject and the compaction keeps
-- exactly the current record.
--
-- `lists = 0` is a VERDICT rather than an absence — the partition has no index
-- and every search scans — with `why` saying which: too small to train, or an
-- index that would have had to read more than half its lists to meet the
-- recall floor. `largest` is the most rows the training filed in one list,
-- which the imbalance rule judges later growth against. The measurement
-- columns are the LATEST measurement — the training's, or a later measure
-- record's — against the exact scan, NULL `recall` when nothing was measured,
-- and `ivf_recall_below_floor` reads them. `measure_position` is the packed
-- position of the record that set them, so an older measurement never replaces
-- a newer one. `digest` is the sha256 of the centroids, which is what the
-- applier and the probe key their decoded copy on.
--
-- # Why the head is NARROW and the centroids have a table of their own
--
-- Every embed apply reads this row to file its vector, and every search reads
-- it to choose its lists. The centroids are up to three quarters of a megabyte
-- at IVFMaxLists and 3 072 dimensions, and a row is read WHOLE: selecting only
-- the columns before a blob still walks its overflow pages. With the blob in
-- this row a node catching up half a million embeds spent minutes reading
-- centroids it had already decoded (BenchmarkIndexHeadRead has the per-read
-- figures), so the blob lives in kb_ivf_centroids and is read only when the
-- decoded copy a process holds is not the one the digest names.
--
-- Class `Divergent`, like kb_vectors: written only by applying a committed
-- record and carried inside a snapshot, so a node that adopts one adopts the
-- index with it.
CREATE TABLE kb_ivf (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    generation       INTEGER NOT NULL,
    log              TEXT    NOT NULL,
    basis            INTEGER NOT NULL,
    seed             INTEGER NOT NULL,
    model            TEXT    NOT NULL,
    dim              INTEGER NOT NULL,
    lists            INTEGER NOT NULL,
    probes           INTEGER NOT NULL,
    trained_on       INTEGER NOT NULL,
    largest          INTEGER NOT NULL DEFAULT 0,
    why              TEXT    NOT NULL DEFAULT '',
    digest           TEXT    NOT NULL DEFAULT '',
    trained_at       INTEGER NOT NULL,
    measured_sources INTEGER,
    recall           REAL,
    floor            REAL,
    shape            TEXT,
    shape_source     TEXT,
    head_misses      INTEGER NOT NULL DEFAULT 0,
    measured_at      INTEGER,
    measure_position INTEGER NOT NULL
);

-- kb_ivf_centroids — the installed index's centroids, beside the digest that
-- names them (see above for why they are not in the head). No row while the
-- partition has no index. `Divergent`, for kb_ivf's reason.
CREATE TABLE kb_ivf_centroids (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    digest    TEXT NOT NULL,
    centroids BLOB NOT NULL
);

-- kb_ivf_rollout — the installed index's re-filing, cut ONCE by its training
-- and carried in its centroids record: batch n re-files the ids of `source` in
-- [from_id, to_id), an empty bound being the source's start or end. A reassign
-- record names only its batch number, so every publication of a batch re-files
-- exactly this range — the property the rollout's coverage rests on
-- (internal/search/ivfrecord.go). `Divergent`, for kb_ivf's reason.
CREATE TABLE kb_ivf_rollout (
    batch   INTEGER PRIMARY KEY,
    source  TEXT NOT NULL,
    from_id TEXT NOT NULL,
    to_id   TEXT NOT NULL
);

-- kb_ivf_lists — how many rows of each source are filed in each list of the
-- installed index: exactly `COUNT(*) FROM kb_vectors_bin WHERE ivf_gen = the
-- installed generation GROUP BY ivf_list, source`, kept by the applier in the
-- same transaction as every row it files, re-files or removes. No row for a
-- count of zero, so every holder's table is the same rows.
--
-- It is what lets a search narrowed to one source know how many of that
-- source's rows each list holds before it reads any of them — the probe reads
-- lists until it has seen as many matching rows as an unfiltered probe reads,
-- or scans — and what lets the embedding duty judge its lists' balance without
-- walking the covering index on every tick. `Derived`: a function of
-- kb_vectors_bin and kb_ivf, both in this file.
CREATE TABLE kb_ivf_lists (
    list   INTEGER NOT NULL,
    source TEXT    NOT NULL,
    filed  INTEGER NOT NULL,
    PRIMARY KEY (list, source)
);
