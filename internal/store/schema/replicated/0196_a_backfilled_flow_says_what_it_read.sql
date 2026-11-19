-- A change of an item's flow derived from its past says which facts it was read from, and is dated by that past (ADR-0162).
-- WHO HAS TO AGREE ON IT: every data node, identically; copied from a backfilled `flow` record's `past` by the tracker's applier, and only a record at version 21 (worklet.RecordVersion) carries one, so a build before it retains such a record rather than applies it.
-- `seats_read` is 1 where the seats' live facts were read: always for a change the flow duty read as it happened, and every row written before this column, which only the duty wrote. A backfilled row's `recorded_at` is the instant its past gives, not its broker's.
ALTER TABLE tracker_flow ADD COLUMN seats_read INTEGER NOT NULL DEFAULT 1;
CREATE INDEX tracker_flow_backfilled_idx ON tracker_flow (task_id) WHERE backfilled = 1; -- the backfill's cursor: it walks the items in id order, so the highest item it recorded is how far it has come
CREATE INDEX tracker_flow_read_idx ON tracker_flow (log_seq) WHERE backfilled = 0;        -- the backfill's horizon: the duty's first change read as it happened, before which an item's past is the backfill's
