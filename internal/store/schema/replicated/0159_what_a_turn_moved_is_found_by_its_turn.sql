-- What a turn moved is read by the turn's id.
--
-- The advisor asks, of the turns a schedule's fires started, which ones moved
-- any work: a tracker or page change other than a comment or a watcher, a
-- push, a change opened, or a review given (each domain's turnsMovedStatement).
-- Each row carries the writing turn's id and nothing indexed it, so the read
-- scanned the whole table. Each index holds only rows a turn wrote.
--
-- WHO HAS TO AGREE ON IT: nobody. An index moves no row.
CREATE INDEX tracker_history_turn_idx ON tracker_history (turn_id, kind) WHERE turn_id != ''; -- read by worklet's Reader.TurnsMoved (turnmoves.go)
CREATE INDEX pages_history_turn_idx ON pages_history (turn_id, kind) WHERE turn_id != '';     -- read by doclet's Reader.TurnsMoved (turnmoves.go)
CREATE INDEX code_pushes_turn_idx ON code_pushes (turn_id) WHERE turn_id != '';               -- read by gitlet's Reader.TurnsMoved (turnmoves.go)
CREATE INDEX code_changes_turn_idx ON code_changes (created_turn) WHERE created_turn != '';   -- read by gitlet's Reader.TurnsMoved (turnmoves.go)
CREATE INDEX code_reviews_turn_idx ON code_reviews (turn_id) WHERE turn_id != '';             -- read by gitlet's Reader.TurnsMoved (turnmoves.go)
