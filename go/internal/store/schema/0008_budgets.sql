-- token_budget_usage — "how much has the company already spent?"
--
-- USAGE ONLY. The CAPS stay config-derived and in memory, because they
-- belong to an epoch: a revision that raises a ceiling should take effect
-- on the next turn, and a limit copied into a row would need its own
-- migration path every time an operator edited the config. What has to be
-- shared is the counter — every node of a fleet spends against one budget,
-- and per-process counters mean N nodes each get the whole allowance.
--
-- The check and the increment are ONE statement (INSERT … ON CONFLICT DO
-- UPDATE … WHERE), so there is no read-then-write window for a peer to
-- slip through. Two nodes racing the last thousand tokens of a cap: one
-- wins, one is refused, and neither overspends.
--
-- Polarity: FAIL CLOSED, which is the opposite of the delivery ledger
-- beside it and deliberately so. A store that cannot be reached must not
-- silently un-cap a company's spending — money leaves the building for
-- every token, and a refused turn is recoverable where an unbounded one is
-- not. The caller distinguishes the two: a refusal is a budget event, an
-- unreachable counter is an error.
--
-- There is no reset schedule here. A budget is a ceiling for the life of a
-- deployment, and an operator who wants a monthly one resets the scope
-- deliberately — a table that rolled itself over would silently re-arm a
-- company somebody had stopped on purpose.
CREATE TABLE token_budget_usage (
    -- 'org' for the company-wide counter, 'agent:{id}' for one seat.
    -- A string rather than two columns because the two scopes are read
    -- and written identically and only ever by exact key; splitting them
    -- would double every statement to say the same thing.
    scope       TEXT    NOT NULL PRIMARY KEY,
    used_tokens INTEGER NOT NULL DEFAULT 0 CHECK (used_tokens >= 0),
    updated_at  INTEGER NOT NULL
);
