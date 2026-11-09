-- A notice says whether it started its recipient's turn, and the ready work a
-- seat is handed again keys on the newest notice to it that did.
--
-- WHO HAS TO AGREE ON IT: tracker_notifications is divergent (outside the
-- identity claim), so a node on an older build that leaves the default never
-- makes two copies of a claimed table disagree.
--
-- `starts_turn` is the decision the notifier made for the recipient
-- (worklet.Heard, then routing.Waking): 1 woke it, 0 told it only. A row an
-- older build wrote reads 1, as every change by somebody else re-armed before.
ALTER TABLE tracker_notifications ADD COLUMN starts_turn INTEGER NOT NULL DEFAULT 1;
CREATE INDEX tracker_notifications_woke_idx ON tracker_notifications (recipient, task_id, starts_turn, log_seq DESC); -- read by the ready work's moved key (ready.go)
