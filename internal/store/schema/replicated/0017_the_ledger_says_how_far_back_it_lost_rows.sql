-- The operation ledger says how far back it may have lost rows.
--
-- # What was missing
--
-- A domain's `<domain>_ops` table is what makes a retry safe: a retry of an
-- operation whose row is present is answered with the first copy, and one
-- whose row is ABSENT is decided and published as a new operation. That reading
-- of absence is sound only where the ledger has never lost a row it once held,
-- and it does lose them — the retention sweep deletes every row applied more
-- than thirty days ago, so "this operation never applied here" and "it applied
-- here and its row was deleted" became the same bytes, and a retry of the
-- second was published a second time.
--
-- # What a row says
--
-- `lost_before` is, per operation ledger, the instant before which that ledger
-- MAY have lost rows: every row it no longer holds was applied before it. An
-- operation minted at or after it has every copy applied at or after its mint,
-- so its row cannot be among the lost, and absence is conclusive; one minted
-- before it may have been, and only its row can speak. No row means the ledger
-- has lost nothing. Who writes it, and why every write moves it only forward,
-- is `internal/statelog`'s to say — see Rows.LostBefore.
--
-- # Why this estate, and why it travels
--
-- In the same file as the ledgers it describes, so whatever deletes ledger rows
-- records the loss in the SAME TRANSACTION as the delete: a watermark written
-- in another file could trail the rows actually gone, and trailing is the
-- unsafe direction. It is the framework's own table, like `statelog_cursor`,
-- and nothing scrubs it, so a donated snapshot carries the donor's value to
-- the node that adopts it.
CREATE TABLE statelog_ops_lost (
    ops_table   TEXT    NOT NULL PRIMARY KEY,
    lost_before INTEGER NOT NULL
);
