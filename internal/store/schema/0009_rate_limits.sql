-- rate_limits — the notification valve, shared across the fleet.
--
-- The pathology it exists for is a notification LOOP: seat A wakes seat B,
-- B's reply wakes A, and the two spend the company's budget on each other
-- until somebody notices. A per-process counter cannot see it — the loop
-- bounces between nodes, so no single process observes enough of it to
-- trip — and worse, N replicas each get the full allowance, so the
-- effective limit is silently N times what the operator wrote.
--
-- A FIXED window, not a sliding one. The counter is keyed by the window a
-- request falls into, so one statement both increments and decides. A
-- sliding window needs a row per event and a range count — more storage
-- and a heavier query, for a valve whose job is to notice runaway volume
-- rather than to meter it precisely. The known cost is a burst of up to
-- 2x the limit straddling a boundary, which is the difference between a
-- loop and a busy second either way.
--
-- The counter STOPS at the limit rather than climbing: the increment
-- carries the ceiling on its WHERE, so a storm updates nothing once it is
-- over. How many were refused is a question the skip records answer, and
-- answering it here would mean a row that grows without bound for exactly
-- as long as the incident lasts.
--
-- Off by default (notification_rate_limit: 0), so a deployment that never
-- asks for the valve never reads this table.
--
-- Polarity: FAIL OPEN, like the delivery ledger and unlike the budget. A
-- valve that cannot be reached must not stop real notifications — an
-- unsent message is work nobody ever does, while an extra one is noise.
CREATE TABLE rate_limits (
    -- What is being limited: 'notify:{agent id}' for the per-seat valve.
    -- A string for the same reason token_budget_usage.scope is one — the
    -- rows are read and written identically and only ever by exact key.
    bucket       TEXT    NOT NULL,
    -- The floor of the window this count belongs to, in microseconds, so
    -- it compares against a wall of elapsed time the way the retention
    -- sweep needs. A window INDEX would only be comparable to other
    -- indices of the same width.
    window_start INTEGER NOT NULL,
    hits         INTEGER NOT NULL DEFAULT 0 CHECK (hits >= 0),
    PRIMARY KEY (bucket, window_start)
);

-- The retention sweep is a range delete over window_start. A window older
-- than one width can no longer affect an answer, so the sweep is pure
-- housekeeping — but it must honour its cutoff rather than clearing the
-- table, or every sweep would reset the LIVE window and let a full
-- limit's worth through again, turning housekeeping into a periodic hole
-- in the rate limit.
CREATE INDEX rate_limits_window_start_idx ON rate_limits (window_start);
