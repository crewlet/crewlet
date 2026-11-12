-- A given-up train's checks are found to cancel.
--
-- A target's merge queue tests a TRAIN: its home queues the required checks
-- on each candidate's tip (purpose candidate) and, bisecting a red batch, on
-- the batch's commits (purpose bisect), each attempt naming the candidate it
-- was queued for (code_checks.candidate_id). Several records give a train up
-- — an eject, a dequeue, a target a person moved, a new candidate record —
-- and their applies drop its candidates and put its entries back to waiting,
-- which leaves its attempts queued or running on commits nothing tests,
-- holding the runners' cells ahead of the live train's, since a runner claims
-- oldest first. The home's queue worker cancels them (internal/gitlet/home's
-- queue.go), and these indexes are how it finds them.
--
-- code_checks_train_idx is a repository's attempts a merge queue queued that
-- have not ended, PARTIAL on those rows alone — an attempt naming a candidate
-- (a tip's bisect over its landings names none) still queued or running —
-- keyed by the repository, the target and the candidate, then the rest of
-- code_checks' key: the home's `queue_due` read of a repository seeks it, and
-- its listing of the repositories owing a cancellation walks it by its
-- leading column. It holds at most a live train's attempts a target — depth
-- times the required checks, and a probe's beside them — and the given-up
-- ones until they are cancelled; an attempt leaves it the moment it ends.
--
-- code_queue_commit_idx is a repository's entries a train is testing or
-- landed, by the target and the commit the train built of each, PARTIAL on
-- those rows: an attempt is GIVEN UP where its commit is neither — a train
-- built again from the same rows is the same commits byte for byte, and its
-- dispatch of one an attempt already measures is answered from that row,
-- which names the candidate it was first queued for, so the commit and not
-- the candidate says whether a train still tests it; and a landed commit's
-- checks are the target's tip work's to read. One seek an attempt.
--
-- code_queue_dequeued_idx is a repository's dequeued entries a train tested,
-- by the candidate that tested each, PARTIAL on those rows — a dequeue keeps
-- the candidate its entry was tested in: a given-up attempt whose candidate
-- merged a change that left its queue is cancelled `dequeued` rather than
-- `abandoned`. One seek an attempt.
--
-- Class: code_checks and code_queue stay REPRODUCIBLE: an index adds no row.

CREATE INDEX code_checks_train_idx ON code_checks (repo_id, target_ref, candidate_id, commit_oid, name, attempt)
    WHERE state IN ('queued', 'running') AND candidate_id != '';    -- a queue's attempts not yet ended, by candidate: the home's cancellation of a given-up train's

CREATE INDEX code_queue_commit_idx ON code_queue (repo_id, target_ref, commit_oid)
    WHERE state IN ('testing', 'landed') AND commit_oid != '';    -- the commits a train tests or landed: whether a queue's attempt is given up

CREATE INDEX code_queue_dequeued_idx ON code_queue (repo_id, candidate_id)
    WHERE state = 'dequeued' AND candidate_id != '';    -- the dequeued entries a train tested, by candidate: a given-up attempt's reason
