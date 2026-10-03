-- The company is read by WHEN things happened: the task history over the rows
-- that change what a company-wide chart or feed draws, and the schedules' runs.
--
-- # The two readers
--
-- `work_flow` (tracker.Reader.Flow) answers "how many tasks were in each status
-- group at the end of each of the last N days or weeks, and how many were
-- completed in each": it takes today's census and walks the history BACKWARD
-- over the window, undoing every change that moved a task between groups, into
-- or out of existence, or between projects. `company_feed`'s tracker half
-- (tracker.Reader.CompanyFeed) pages completions, creates and hand-offs newest
-- first. Both select on the instant a change took effect and on nothing that
-- narrows them first — no subject, no project, no position — so without this
-- index both are a scan of `tracker_history`, a table that is never swept and
-- grows for the life of the company.
--
-- # Why `effective_at` and not the authored instant or the position
--
-- `effective_at` is the fleet-agreed instant, the one every duration and every
-- report window is measured on (0002's annotation), so a day cut on it is the
-- same day on every node. The position is a total order but not a clock, and a
-- day boundary is a clock question. The authored instant is what the writer
-- typed and can run backwards across writers.
--
-- `raiseSuccessors` rewrites `effective_at` upward when a late record lands
-- below rows already applied, and each such rewrite now moves this index's
-- entry too — the write cost 0008 named when it dropped the last index on this
-- column for having no reader. It has two now.
--
-- # Why partial, and why over these expressions
--
-- Most commits change nothing either reader draws: a title, a tag, a comment, a
-- due date. The predicate is the one both readers state verbatim, so the
-- planner proves the implication term for term and SEARCHes on the index
-- (asserted by tracker's TestTheFlowAndFeedReadsSearchTheirIndex): a task row
-- whose status, assignee or project moved, or which was created, removed,
-- restored or purged. The deltas are read out of `fields_json`, the applier's
-- own record of what moved, for 0008's reason — a second column would be a
-- second copy of a fact the row already holds.

CREATE INDEX tracker_history_moves_idx ON tracker_history (effective_at, log_seq)
    WHERE subject_kind = 'task' AND (
        kind IN ('created', 'removed', 'restored', 'purged')
        OR json_extract(fields_json, '$.status.to') IS NOT NULL
        OR json_extract(fields_json, '$.assignee.to') IS NOT NULL
        OR json_extract(fields_json, '$.project.to') IS NOT NULL);  -- work_flow's backward walk and company_feed's tracker page

-- # And the schedules' runs, the feed's third source
--
-- `company_feed` pages `usage_schedule_runs` newest first by `fired_at`. The
-- table's key leads with the company DAY, which bounds a range but orders
-- nothing inside one, so without this every page sorted the whole 181-day
-- horizon the usage applier keeps (0023) — an hourly schedule is 4,344 rows
-- of it.

CREATE INDEX usage_schedule_runs_fired_idx ON usage_schedule_runs (fired_at);  -- company_feed's schedule page
