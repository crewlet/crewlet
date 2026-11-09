-- A notice says whether it started its recipient's turn, and the ready work a
-- seat is handed again keys on the newest notice to it that did.
--
-- WHO HAS TO AGREE ON IT: every node applying the same record writes the same
-- value. `starts_turn` is the notifier's decision (worklet.Heard, then
-- routing.Waking) only on a record at version 7 that marks its notices keep it
-- (MutationRecord.NoticeWokeKept); every other record, and every row an older
-- build wrote, reads 1, as every change by somebody else re-armed before. A
-- build older than version 7 retains a marked record rather than apply it.
ALTER TABLE tracker_notifications ADD COLUMN starts_turn INTEGER NOT NULL DEFAULT 1;
CREATE INDEX tracker_notifications_woke_idx ON tracker_notifications (recipient, task_id, starts_turn, log_seq DESC); -- read by the ready work's moved key (ready.go)
