-- rate_limits — shared sliding-window counters.
--
-- Backs `notification_rate_limit`, the per-agent-per-second valve
-- against webhook storms and notification loops. Per-process counters
-- multiply the effective limit by replica count, and they miss the
-- pathology the valve exists for most completely: a notification LOOP
-- (A wakes B, B wakes A) bounces between nodes, so no single process
-- ever sees enough of it to trip.
--
-- One row per (bucket, window start). Rows are disposable — the sweep is
-- a range delete on window_start — because this answers "how many in the
-- last second", not "what happened".

CREATE TABLE IF NOT EXISTS rate_limits (
    bucket       TEXT        NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    count        INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, window_start)
);

CREATE INDEX IF NOT EXISTS rate_limits_window_idx ON rate_limits (window_start);
