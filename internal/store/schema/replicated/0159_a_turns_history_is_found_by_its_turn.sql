-- What a turn wrote to the tracker is read by the turn's id.
--
-- The advisor asks, of the turns a schedule's fires started, which ones moved
-- any work (worklet.turnsMovedStatement). The history rows carry the writing
-- turn's id and nothing indexed it, so the read scanned the whole history.
--
-- WHO HAS TO AGREE ON IT: nobody. An index moves no row.
CREATE INDEX tracker_history_turn_idx ON tracker_history (turn_id, kind) WHERE turn_id != ''; -- read by Reader.TurnsMoved (turnmoves.go)
