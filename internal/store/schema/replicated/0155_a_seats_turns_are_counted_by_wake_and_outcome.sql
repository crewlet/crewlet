-- A seat's wakes are counted by what woke them, what became of them and what
-- the turns they started did.
--
-- usage_turn_ledger: one row per (day, node, seat, wake, disposition, outcome).
-- A wake is the object's kind and reason, a schedule by its name, or the waking
-- event's type. Its disposition is `started` (a turn ran; outcome is `moved`,
-- `words` or `nothing`, read from the kinds of change its writes committed, and
-- the row carries what the turns spent, their in-turn auxiliary calls apart) or
-- `held` (the seat was told and not woken; no outcome and no spend). Started
-- rows are read off each turn's ending record, which states the whole turn
-- (types.TurnTally); held rows off the `wake_held` records.
--
-- usage_tool_refusals: one row per (day, node, seat, server, tool, class): the
-- calls the engine refused under that refusal class in the turns that ended
-- that day, the engine's own tools under an empty server.
--
-- Divergent, like every row table of this domain. The horizon is the applier's,
-- by the record's day, as 0023 states. Both are keyed on the day first, which is
-- what a range of days reads, so neither needs an index of its own.
CREATE TABLE usage_turn_ledger (
    day         TEXT    NOT NULL,
    node        TEXT    NOT NULL,
    agent_id    TEXT    NOT NULL,
    wake        TEXT    NOT NULL,
    disposition TEXT    NOT NULL,
    outcome     TEXT    NOT NULL DEFAULT '',
    turns       INTEGER NOT NULL DEFAULT 0,
    input       INTEGER NOT NULL DEFAULT 0,
    output      INTEGER NOT NULL DEFAULT 0,
    cache_read  INTEGER NOT NULL DEFAULT 0,
    cache_write INTEGER NOT NULL DEFAULT 0,
    aux_input   INTEGER NOT NULL DEFAULT 0,
    aux_output  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, node, agent_id, wake, disposition, outcome)
);

CREATE TABLE usage_tool_refusals (
    day      TEXT    NOT NULL,
    node     TEXT    NOT NULL,
    agent_id TEXT    NOT NULL,
    server   TEXT    NOT NULL DEFAULT '',
    tool     TEXT    NOT NULL,
    class    TEXT    NOT NULL,
    calls    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, node, agent_id, server, tool, class)
);

-- What a seat-day's caps on its ledger and refusal rows dropped, and how many of
-- its ended turns stated no ledger figures (ended by a build before the ledger),
-- so a regression that stops stamping them reads as a count rather than a quiet day.
ALTER TABLE usage_turns ADD COLUMN ledger_elided INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_turns ADD COLUMN refusals_elided INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_turns ADD COLUMN ledger_untallied INTEGER NOT NULL DEFAULT 0;
