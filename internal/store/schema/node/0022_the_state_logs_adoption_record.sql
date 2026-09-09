-- statelog_adoption records what this node adopted and when it finished.
--
-- IN THE NODE ESTATE, unlike the framework's other two tables, and the reason
-- is what the row is ABOUT: the cursor and the anchor are facts a snapshot
-- carries between nodes, and this is one node's own record of having received
-- one. A donated copy of it would tell the recipient about the donor.
--
-- # What reads it, and why an absent answer is not the same as a zero
--
-- A domain's operation ledger is this node's own and is scrubbed out of every
-- donated snapshot, so a node that adopted one arrives with an empty ledger.
-- An operation minted BEFORE that instant therefore cannot be answered for
-- here at all — and reading the ledger's silence as "somebody else won" would
-- make a writer re-decide against a row that moved because of its own write.
-- So a write whose operation predates the newest completed adoption resolves
-- `unknown` carrying its operation id, rather than being republished.
--
-- Narrow: the node must have had a write in flight before falling below the
-- trim floor stopped it writing. It gets a table rather than a footnote
-- because it is the exact failure the ledger exists to prevent, arriving
-- through the one door the ledger cannot watch.
--
-- Rows ACCUMULATE. A node may adopt more than once over its life, and what an
-- operator wants when a node is behaving oddly is the history rather than the
-- latest row; the reader takes the newest completed one.
CREATE TABLE statelog_adoption (
    -- started_at is when the adoption began, and is the key because it is the
    -- one value present from the first moment there is a row at all.
    started_at   INTEGER NOT NULL PRIMARY KEY,

    -- donor is the node the artefact came from, for the operator reading a
    -- fleet that disagrees with itself.
    donor        TEXT    NOT NULL,

    -- manifest is the artefact's own identity — its checksum — so a node can
    -- say which copy it is running rather than only when it took it.
    manifest     TEXT    NOT NULL,

    -- completed_at is NULL until the adoption finishes, which is what makes an
    -- interrupted adoption visible as an interrupted adoption rather than as a
    -- node that has always been here.
    completed_at INTEGER
);
