-- a2a_channels — who is on an agent-to-agent channel, and is it open.
--
-- This state used to be a dict on `A2AService`, which is process-local.
-- Every authorization decision the service makes reads it: may this
-- sender post here, may this closer close it, who is the other party.
-- On one node that works. On two it fails in the direction that looks
-- like a bug in the agent rather than in the engine — the target wakes
-- on the node that owns ITS seat, which is rarely the node that opened
-- the channel, so `send` there raises "channel is not open or does not
-- exist" for a channel that very much exists.
--
-- `state` is two-valued and always will be. A channel is open until
-- somebody closes it or it ages out; there is no in-between worth
-- recording, and the seat leases already decide who may act on either
-- side. (See `turn_completions` for the same argument made at length.)
--
-- Rows outlive the conversation deliberately: `closed` is the answer to
-- "why did my reply bounce", and a channel that vanished on close would
-- make a late reply indistinguishable from a typo'd channel id. The
-- MaintenanceWorker deletes them on a retention horizon instead.

CREATE TABLE IF NOT EXISTS a2a_channels (
    channel_id   TEXT        PRIMARY KEY,
    requester    TEXT        NOT NULL,
    target       TEXT        NOT NULL,
    state        TEXT        NOT NULL DEFAULT 'open',
    opened_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at    TIMESTAMPTZ,
    message_count INT        NOT NULL DEFAULT 0
);

-- Retention is a range delete over opened_at, run by the
-- MaintenanceWorker; the partial index serves the TTL close of
-- abandoned channels, which only ever scans open ones.
CREATE INDEX IF NOT EXISTS a2a_channels_opened_at_idx
    ON a2a_channels (opened_at);
CREATE INDEX IF NOT EXISTS a2a_channels_open_idx
    ON a2a_channels (opened_at) WHERE state = 'open';
