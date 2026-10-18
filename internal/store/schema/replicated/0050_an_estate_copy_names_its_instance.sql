-- statelog_estate — WHICH COPY of the replicated estate this file is.
--
-- THE FRAMEWORK'S, and the one framework row that is not derived from any log:
-- every other row in this file is the same on every data node at one position,
-- and this one names the FILE rather than anything the file says. An adoption
-- mints it a random instance id for every snapshot it installs, BEFORE the
-- rename puts the snapshot in place (internal/statelog's Adopter) — so the row
-- a running estate holds changes exactly when the rows under a node were
-- replaced by something other than its own appliers.
--
-- # Who reads it, and why a row and not a signal
--
-- What a node keeps DERIVED from the replicated rows by a walk that only moves
-- forward — the chat keyword index, in the node's own file, walking
-- `chat_messages.version` from a durable watermark — cannot see a row below
-- that watermark being replaced. The engine says so when it replaces the
-- estate under a running node, but a signal is held in memory: a process that
-- stopped between the install and the next sweep took it with it, and the
-- walk resumed over the donor's rows from a watermark built over the old
-- copy's. So the derived index records which instance it was built from, and
-- every sweep compares that with this row; a mismatch is a replacement,
-- whenever it happened and whoever was running.
--
-- # Minted on adoption, and empty on a file these migrations created
--
-- MINTED, never copied: an artefact carries its donor's instance, and a node
-- that adopted twice from one donor would otherwise come back to an instance
-- its index had already recorded.
--
-- A FILE CREATED HERE IS NAMED THE EMPTY STRING, the same on every such file.
-- The name is only ever compared WITHIN one node — a derived index against the
-- estate beside it — and a file these migrations created holds what this
-- node's own appliers wrote from the logs, which an index walking those logs
-- follows; one created after an adopted copy is still a different name from
-- that copy's. A name minted here would be a migration writing something
-- particular to the file it ran in, which every store seeded from one migrated
-- image would then share (internal/store/storetest holds the image to a fresh
-- migration row for row). What KEEPS an instance is a copy restored from a
-- backup beside the node file of the same set (docs/guides/backup.md) — the one
-- replacement whose derived index came with it.
--
-- ONE ROW, `id = 1`, enforced by the key. No sweep: it is one row.
CREATE TABLE statelog_estate (
    id         INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    instance   TEXT    NOT NULL,
    -- When the adopter last stamped it, and 0 for a file these migrations
    -- created: an operator reading why a node rebuilt its chat index reads
    -- the instant the estate under it was replaced.
    stamped_at INTEGER NOT NULL
);

INSERT INTO statelog_estate (id, instance, stamped_at) VALUES (1, '', 0);
