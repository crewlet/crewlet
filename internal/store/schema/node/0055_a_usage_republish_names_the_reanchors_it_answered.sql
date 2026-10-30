-- usage_republished — the usage log's generation on every data node, as this
-- node's usage publisher last republished every day its event log holds
-- under.
--
-- WHO HAS TO AGREE ON IT: this node alone. It is a fact about which of the
-- fleet's reanchors THIS node's publisher has answered, and only this node
-- publishes its days (internal/usage, ADR-0020).
--
-- # Why a reanchor anywhere owes every day again
--
-- A reanchor of a RESTORED usage log follows the log from its END: replayed
-- into a new generation, the copy's older records would outrank every row and
-- roll each day back. So the node that re-anchored never applies the records
-- written after the restore and before its reanchor — and a day another data
-- node republished in that tail, and has not moved since, stays at its
-- pre-restore value on the re-anchoring node, since a publisher republishes a
-- day only when it moves. Each data node's usage log is re-anchored on its
-- own, so the move is visible to the others only as that node's generation
-- in the positions register. A publisher that sees any data node's
-- generation rise past the one it last answered republishes every day its
-- event log still holds, under operation ids that cannot collapse onto a copy
-- written before the move, and records here the generations it answered once
-- every one of those days has settled.
--
-- HELD ACROSS A RESTART because the move is seen once: a node down while a
-- peer re-anchored reads the register's generations on its first beat, and
-- without this row it could not tell them from the ones it already answered.
--
-- ONE ROW, `id = 1`, in the publisher's own canonical spelling (`node@gen`,
-- by node, comma-joined). No sweep: it is one row.
CREATE TABLE usage_republished (
    id          INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    generations TEXT    NOT NULL
);
