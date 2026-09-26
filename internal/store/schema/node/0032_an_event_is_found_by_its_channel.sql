-- The event log's channel filter becomes an index seek.
--
-- `channel_id` has been a promoted column since schema/0001 — the writer
-- copies it out of every A2A event's payload (store.ExtractTags) — and until
-- now nothing could ask for it: `/events?channel_id=` did not exist, so the
-- one question an agent-to-agent conversation's page asks of the log, "what
-- happened on this channel", had no answer. The filter this ships beside
-- makes it askable, and an unindexed filter on it would walk the primary key
-- newest first until it had a page — which, for a channel with fewer rows than
-- a page (every channel: an A2A conversation is a handful of events), is the
-- whole thirty-day log on every click.
--
-- WHO HAS TO AGREE ON IT: this node alone. It is an index over a column of the
-- node's own audit log, derived by the writer from the event it stores, exactly
-- as the turn, work-key and work-item indexes are.
--
-- NEWEST FIRST, (channel_id, event_time DESC, event_id DESC), because that is
-- the listing's own ORDER BY — the shape every filter index in 0001 has and for
-- the reason it gives: an index that stops at the time forces a sort over the
-- whole matching set to resolve the id tiebreak.
--
-- PARTIAL over the rows that name a channel, the shape 0029 and 0031 gave
-- work_key and work_item: '' is the absence of a channel rather than one, and
-- every chat delivery, phase record, webhook and fleet event would otherwise be
-- an index entry pointing at nothing. No backfill: the column has been written
-- since the table was created.
CREATE INDEX IF NOT EXISTS crewlet_events_channel_idx
    ON crewlet_events (channel_id, event_time DESC, event_id DESC)
    WHERE channel_id <> '';
