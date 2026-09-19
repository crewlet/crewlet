-- The two history feeds get an index on WHO WAS WRITING, and the pages feed
-- gets one on its own order at all.
--
-- # What this is for
--
-- `#/admin/audit` is one question: what did the OPERATORS of this company do
-- to it. Not one seat's work and not one page's history — every commit whose
-- writer was a token or a person, across the tracker and the knowledge base,
-- newest first. Both feeds already carry `actor_kind` on every row; nothing
-- could filter on it, and nothing could have afforded to.
--
-- # Why an index rather than a filter
--
-- `tracker_history` is one row per applied commit and NOTHING SWEEPS IT: a
-- question about last week and one about three years ago read the same table.
-- The existing indexes are on the subject, the project, the kind, the comment,
-- the batch and the log position — so `WHERE actor_kind = 'operator' ORDER BY
-- log_seq DESC LIMIT 200` had no index to take and answered by reading every
-- commit the company has ever made into a temp b-tree. On a company at any
-- real age that is not a slow screen, it is a screen that times out, and the
-- feed would have been built by paging the whole log into the browser and
-- filtering it there — which is a different answer wearing the same shape,
-- because a page of 200 commits narrowed client-side to the operator's three
-- is a screen that says "nothing" on a company whose operators are busy.
--
-- `(actor_kind, log_seq DESC)` makes one kind a RANGE, and the feed's own
-- newest-first order comes out of the index rather than out of a sort. A set
-- of kinds is one range each.
--
-- # And the pages feed had no index on its order at all
--
-- Found while writing the one above, and it is the older defect. `pages_history`
-- ships two indexes — `(page_id, created_at DESC)` for one page's activity, and
-- `(created_at)` for "the company-wide digest". But the digest does not order by
-- `created_at`: `readPageActivity` orders by `version DESC` and pages on
-- `version <`, because the version is the composed LOG POSITION and the created
-- instant is a clock two nodes can disagree about. So the index that names the
-- digest in its own comment cannot serve it, and the digest — the Knowledge
-- screen's own feed, on every load — has been a full scan of every page change
-- the company has made, sorted in a temp b-tree, since the domain landed.
--
-- The `created_at` index is left alone: it is what a wall-clock window over the
-- feed would take, and removing it is a separate decision from adding the one
-- the feed actually needs.

CREATE INDEX tracker_history_actor_kind_seq_idx
    ON tracker_history (actor_kind, log_seq DESC);        -- work_activity(actor_kinds=), which is the audit feed

CREATE INDEX pages_history_version_idx
    ON pages_history (version DESC);                      -- page_activity's company-wide digest, which orders by the log position

CREATE INDEX pages_history_actor_kind_version_idx
    ON pages_history (actor_kind, version DESC);          -- page_activity(actor_kinds=), the audit feed's other half
