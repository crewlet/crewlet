-- Fence a detached sandbox run to the node that owns its seat.
--
-- A run outlives the process that started it: the box is real and
-- billed, the row is the durable state, and the seat it belongs to can
-- move between nodes while the coding agent is still working. Without an
-- owner on the row, any node could resume, reap or settle any run --
-- which is exactly what a lease exists to prevent one level up.
--
-- `owner` is a process INCARNATION (`{node_id}:{random}`), matching
-- `leases.owner`, so a restarted node with the same node_id does not
-- inherit its predecessor's claim. `owner_epoch` is the seat lease's
-- monotonic epoch at the moment of the claim: the fencing token. Every
-- mutation that acts on a live run carries `WHERE owner_epoch = $mine`,
-- so a node whose lease moved cannot write even if it has not yet
-- noticed -- the ownership check in the handler is an optimization, the
-- fence in the write is the guarantee.
--
-- NULL means unclaimed, which is what an in-flight run looks like the
-- instant before its seat's owner recovers it. Existing rows are NULL by
-- definition: they were written before ownership existed, so the first
-- node to claim their seat adopts them.
ALTER TABLE pending_sandbox_run
    ADD COLUMN IF NOT EXISTS owner       TEXT,
    ADD COLUMN IF NOT EXISTS owner_epoch BIGINT;
