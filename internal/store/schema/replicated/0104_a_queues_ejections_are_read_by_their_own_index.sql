-- A queue's ejections are read by their own index.
--
-- A `queue` read of a named repository answers, beside its live entries, what
-- its queues put out in the last day (internal/code's queueread.go,
-- QueueEjectionsShown, at most MaxEjectionsShown), and the dashboard's Queue
-- section asks it every half minute for every reader watching. 0082 gave the
-- read only code_queue_ended_idx, partial on every ENDED entry — landed,
-- ejected and dequeued — so it walked every entry the repository ended in
-- the day, across every branch even where the read names one, and fetched
-- each row to test whether it was an ejection. A healthy queue ejects almost
-- nothing and lands everything, so the read's cost followed the queue's
-- LANDINGS: thousands of rows a day per branch at the queue's capacity to
-- return a handful.
--
-- Two indexes partial on exactly the ejections, one for each form of the
-- read, so each is one seek of the rows it answers and touches at most
-- MaxEjectionsShown of them whatever the landing rate:
-- code_queue_ejected_idx a repository's, newest ended first, and
-- code_queue_target_ejected_idx one branch's. Only an ejection and the
-- horizon's sweep of one write them, so the landings and dequeues pay
-- nothing.
--
-- Class: code_queue stays REPRODUCIBLE: an index adds no row, and the table
-- is written only by the applier from the records on a repository's own
-- subject.
CREATE INDEX code_queue_ejected_idx ON code_queue (repo_id, ended_at)
    WHERE state = 'ejected';    -- a repository's ejections by their ending: the queue read's ejections

CREATE INDEX code_queue_target_ejected_idx ON code_queue (repo_id, target_ref, ended_at)
    WHERE state = 'ejected';    -- one branch's ejections by their ending: the queue read's ejections of a target
