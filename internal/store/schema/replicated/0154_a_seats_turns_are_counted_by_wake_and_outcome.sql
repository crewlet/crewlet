-- A seat's turns are counted by what woke them and what they did.
--
-- One row per (day, node, seat, wake, outcome): the turns that ended on that
-- node's day under that wake (the object's kind and reason, a schedule by its
-- name, or the waking event's type) with that outcome (`moved`, `words` or
-- `nothing`, read from the kinds of change their writes committed), and what
-- they spent, their in-turn auxiliary calls apart. Read off each turn's ending
-- record, which states the whole turn (types.TurnTally).
--
-- Divergent, like every row table of this domain. The horizon is the
-- applier's, by the record's day, as 0023 states.
CREATE TABLE usage_turn_ledger (
    day         TEXT    NOT NULL,
    node        TEXT    NOT NULL,
    agent_id    TEXT    NOT NULL,
    wake        TEXT    NOT NULL,
    outcome     TEXT    NOT NULL,
    turns       INTEGER NOT NULL DEFAULT 0,
    input       INTEGER NOT NULL DEFAULT 0,
    output      INTEGER NOT NULL DEFAULT 0,
    cache_read  INTEGER NOT NULL DEFAULT 0,
    cache_write INTEGER NOT NULL DEFAULT 0,
    aux_input   INTEGER NOT NULL DEFAULT 0,
    aux_output  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, node, agent_id, wake, outcome)
);

-- "What did the company's turns do over these days" is a day range of every
-- node and every seat, which the key's leading day already seeks: no index of
-- its own, so the planner never prefers a day index to the key for one seat.

-- What a seat-day's cap on its ledger rows dropped, beside its tools'.
ALTER TABLE usage_turns ADD COLUMN ledger_elided INTEGER NOT NULL DEFAULT 0;
