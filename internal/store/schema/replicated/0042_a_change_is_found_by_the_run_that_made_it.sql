-- A change is found by the run that made it.
--
-- Every history row the tracker and the pages write names the run that made
-- the change (`turn_id`), and nothing has read it by that column: the task's
-- feed, the page's activity and the company's are all read by their subject or
-- by position. A turn's STORY turns the question round — what did this session
-- leave behind? — and it is asked of a page of runs at once, so without these
-- every story read scanned the whole history of the company's work.
--
-- WHO HAS TO AGREE ON IT: every node, identically — each is an index over a
-- domain's own replicated rows, which its applier writes and nothing else does;
-- an index is not state, and every node builds the same one.
--
-- PARTIAL over the rows that name a run, for the reason 0030's is: '' is a
-- change no turn made — a person's, an operator's, a duty's — which no story
-- read names, and which is most of a busy company's history.
CREATE INDEX tracker_history_turn_idx ON tracker_history (turn_id, log_seq) WHERE turn_id <> '';  -- TurnWrites: what a run changed on the tracker
CREATE INDEX pages_history_turn_idx ON pages_history (turn_id, version) WHERE turn_id <> '';      -- TurnWrites: what a run changed on the pages
