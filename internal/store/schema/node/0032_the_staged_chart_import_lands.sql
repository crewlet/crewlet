-- A chart an OFFLINE import staged for this node to publish at its next boot.
--
-- # The gap this closes
--
-- A company file carries both halves — the settings and the org chart — and
-- `crewlet config import` writes both when it can reach a running node. With
-- the engine STOPPED it can write only one: a revision is a row in this
-- database, and a chart is a record on an ordered log that needs the broker
-- this process did not open.
--
-- That left an operator with a stopped engine and an edited chart with nowhere
-- to put it. The command said so rather than pretending — which is the right
-- half of the answer and not the whole of it, because the gesture they were
-- performing is "make this file the company", and half of it silently did not
-- happen until they went and found a second, different command.
--
-- So the offline route STAGES the chart here, and the next boot publishes it.
--
-- # Why one row, and why it is keyed on the chart's own content
--
-- A stage is a PENDING INTENT rather than a history: two offline imports in a
-- row mean the operator changed their mind, and publishing both would replay a
-- structure they have already abandoned. The key is the same content hash the
-- import ledger uses, so re-staging an unchanged chart overwrites the row it
-- would have duplicated, and the ledger makes the publish itself a no-op on
-- every node if it has already landed.
--
-- PRIMARY KEY (id) over a fixed value, rather than a bare one-row table: it is
-- the shape that makes "replace whatever is staged" one statement, and it
-- leaves the door open to a second stage per node without a migration.
--
-- # It is SEALED, for the reason the revision beside it is
--
-- An authored chart carries every seat's runtime half — its model chain, its
-- sandbox cell, its `mcp_env` — and an offline import has NOT been through the
-- chart writer, which is what turns a literal credential into a sealed
-- reference. So a file holding one would put it in this table in plaintext,
-- where a backup copies it and an operator reading the database finds it. The
-- payload is sealed with the same Tier A keyring `company_config.payload` is.
--
-- # This node's own, which is why it is in the node estate
--
-- A stage is a decision made at ONE machine's command line, about a publish
-- that machine will perform. Replicating it would have every node in the fleet
-- publish the same structure, which the ledger would collapse — after N nodes
-- had each written a record for it.
CREATE TABLE chart_import_staged (
    -- id is the chart's own content hash, exactly as the import ledger keys
    -- on it, so the two agree about what "this chart" is.
    id         TEXT    NOT NULL PRIMARY KEY,
    -- payload is the sealed, encoded chart.Authored value.
    payload    BLOB    NOT NULL,
    -- source_path is the file it came from, for the line the boot logs: an
    -- operator reading "published a staged chart" needs to know which one.
    source_path TEXT   NOT NULL DEFAULT '',
    staged_at  INTEGER NOT NULL,
    staged_by  TEXT    NOT NULL DEFAULT ''
);

-- NO INDEX. The table holds a pending intent and is read whole at boot, one
-- row at a time; an index on a table that is almost always empty is a write
-- cost with no reader.
