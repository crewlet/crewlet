-- custody_unsettled — the batches of a stateless node's events this node has
-- written into its own event log without yet knowing whether it KEEPS them
-- (ADR-0025, internal/observe's custody).
--
-- WHO HAS TO AGREE ON IT: this node alone, and only about rows it wrote. Which
-- data node keeps a batch is the COMPANY's answer and lives in coordination
-- (coord.Custody); what this table remembers is that THIS node wrote a batch
-- and has not heard that answer yet — so a node that crashed between writing a
-- batch and claiming it asks at its next pass, and deletes the rows if another
-- node keeps the batch.
--
-- A ROW PER BATCH IN FLIGHT, and none for a settled one: a batch this node
-- keeps is ordinary rows of its event log once it knows, and one it does not
-- keep is gone from the log with its row. So the table holds what the node
-- wrote in the last few moments and needs no sweep; a batch whose keeper
-- cannot be learned for as long as the log keeps rows is let go with them.
--
-- `events` is the batch's rows as a JSON array of {"t": event_time, "id":
-- event_id} — the event log's own identity, as stored — which is what a
-- release deletes by.
CREATE TABLE custody_unsettled (
    batch_id   TEXT    NOT NULL PRIMARY KEY,
    origin     TEXT    NOT NULL,
    written_at INTEGER NOT NULL,
    events     TEXT    NOT NULL
);
