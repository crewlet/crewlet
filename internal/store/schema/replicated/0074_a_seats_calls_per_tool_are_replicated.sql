-- usage_tool_calls: one row per (day, node, seat, server, tool), the seat's
-- calls to one MCP server's tool in the turns that ended that day on that
-- node, and what became of them. Written only by the usage domain's applier,
-- from a seat record that carries them (internal/usage, record version 2).
--
-- # Who has to agree on it
--
-- Every data node, and only the current value: the usage domain's answer
-- (0023, adr/0020). A seat's calls are counted where its turns are, off the
-- one record that ends each turn (types.TurnTally.Tools), so a node's day of
-- calls is derived from that node's own event log and republished whole like
-- the rest of its day, and a node that left the fleet still answers for the
-- calls its seats made. The calls themselves (which tool, when, what became of
-- each) stay turn detail in the event log of the node that ran them, read
-- across the fleet at query time (adr/0021): this table holds counts, never an
-- argument or a result.
--
-- # Under the seat's head row
--
-- No `version` here: a seat record's guard is its head row in usage_turns, as
-- for its tokens and its reads, and the apply replaces these rows only when
-- that row is older than the record. Capped per seat-day by the WRITER
-- (usage.ToolsPerSeatDay); what the cap dropped is counted on the head row's
-- `tools_elided`, never silently.
--
-- Each figure is a count that only grows, joined by MAX when a node publishes
-- a day the fleet holds more of (usage.Join), so `calls` is not asserted to be
-- the sum of the three outcomes beside it. `total_ms` is how long the calls
-- that ran took between them, and `last_at` when the newest of those was
-- handed to the tool, in the store's instant encoding, 0 when none ran.
--
-- Divergent, like every row table of this domain. The horizon is the
-- applier's, by the record's day, as 0023 states.
CREATE TABLE usage_tool_calls (
    day         TEXT    NOT NULL,
    node        TEXT    NOT NULL,
    agent_id    TEXT    NOT NULL,
    server      TEXT    NOT NULL,
    tool        TEXT    NOT NULL,
    calls       INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    unavailable INTEGER NOT NULL DEFAULT 0,
    refused     INTEGER NOT NULL DEFAULT 0,
    total_ms    INTEGER NOT NULL DEFAULT 0,
    last_at     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, node, agent_id, server, tool)
);

-- "How has this server been used lately" is a server's own question, over a
-- day range of every node and every seat.
CREATE INDEX usage_tool_calls_server_idx ON usage_tool_calls (server, day);

-- What a seat-day's cap on its tool entries dropped, beside what its cap on
-- its reads dropped.
ALTER TABLE usage_turns ADD COLUMN tools_elided INTEGER NOT NULL DEFAULT 0;
