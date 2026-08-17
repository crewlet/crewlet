-- token_budget_usage — the shared half of the token budget.
--
-- A budget has two halves, and only one of them is shared state.
--
-- The CAP (`token_budget` on the org, `role.token_budget` on a seat) is
-- config: every process derives the same number from the same active
-- revision, so it needs no row here.  The USAGE counter is the half that
-- must be atomic, because "have we spent 500k tokens yet" is a question
-- about the company, not about a process.  Kept in memory, an org cap of
-- 500k silently became N x 500k the moment a second process ran.
--
-- `scope` is 'org' or 'agent:{agent_id}'.  Agent ids are the derived
-- uuid5 (see db/agents.py), so a row survives a restart and a rename of
-- anything except the handle or org name.
--
-- The enforcement path is one conditional UPDATE per scope whose WHERE
-- clause carries the cap:
--
--   ... SET used_tokens = used_tokens + $delta
--   WHERE scope = $1 AND ($cap = 0 OR used_tokens + $delta <= $cap)
--
-- Zero rows updated means the spend would breach the cap; no read-then-
-- write window exists for a peer to slip through.  Org and agent are
-- bumped inside one transaction so a turn can never charge the org for
-- tokens its own seat was refused.
--
-- BEHAVIOUR CHANGE: usage is now DURABLE.  It used to reset whenever the
-- engine restarted, which made a cap advisory in exactly the situation
-- that motivates one (an agent burning budget in a crash loop).  Reset
-- deliberately with `crewlet budgets reset`.

CREATE TABLE IF NOT EXISTS token_budget_usage (
    scope       TEXT PRIMARY KEY,
    used_tokens BIGINT      NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT token_budget_usage_used_non_negative CHECK (used_tokens >= 0)
);
