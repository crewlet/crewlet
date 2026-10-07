-- The follows' handoff source goes.
--
-- Migration 0028 moved the thread follows to the coordination store and kept
-- `chat_thread_follows` only so the rows an earlier build had written could be
-- carried onto the fleet at boot. No build this engine supports wrote one —
-- every follow since 0028 is a coordination record, and a node database is
-- only ever created by running every migration from empty — so the table is
-- empty on every database there is, the boot handoff that read it is gone,
-- and a table nothing reads or writes is dropped rather than kept as an
-- invitation to wire something to it.
--
-- 0028 already dropped both of its indexes, so the table is all that is left.

DROP TABLE IF EXISTS chat_thread_follows;
