-- Role/unit-scoped scheduled work (cron analogue).
--
-- Dispatch ledger for the Scheduler. One row per fired (or deliberately
-- skipped) schedule run. The composite PRIMARY KEY plus
-- INSERT ... ON CONFLICT DO NOTHING gives at-most-once delivery across
-- engine restarts and re-ticks.
--
-- Dedup identity is the tuple of columns, NOT a delimiter-joined string,
-- so a ':' (or any character) in a unit name / schedule name / handle
-- can't collide two distinct fires onto one key.
--
-- fire_label is the LOCAL wall-clock stamp (YYYYmmddTHHMM in the
-- schedule's timezone), not the UTC instant. This makes the dedup
-- DST-correct: on a fall-back day a local cron minute maps to two UTC
-- instants, but both share one fire_label, so the run fires once rather
-- than twice. scheduled_at keeps the UTC instant of the first-claimed
-- fire for display.
--
-- Identity:
--   - role schedule:      scope_id = role handle,  target_handle = role handle
--   - unit each-member:   scope_id = unit name,    target_handle = member handle
--   - unit lead/role:     scope_id = unit name,    target_handle = resolved handle
--   - skipped catchup:    target_handle = '' (no runner resolved yet)
--
-- This is a DISPATCH ledger, not a turn-outcome store: outcome is
-- 'fired' or 'skipped_catchup'. The downstream turn result (done /
-- failed / timed out) lives in the normal turn telemetry (TaskStarted /
-- TaskCompleted / TaskFailed, TurnGuardBreach) keyed by the same trace.

CREATE TABLE scheduled_runs (
    scope_type    TEXT        NOT NULL,
    scope_id      TEXT        NOT NULL,
    schedule_name TEXT        NOT NULL,
    fire_label    TEXT        NOT NULL,
    target_handle TEXT        NOT NULL DEFAULT '',
    scheduled_at  TIMESTAMPTZ NOT NULL,
    fired_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    outcome       TEXT        NOT NULL DEFAULT 'fired',
    trace_id      TEXT        NOT NULL DEFAULT '',  -- OTel trace of the turn this fire triggered
    PRIMARY KEY (scope_type, scope_id, schedule_name, fire_label, target_handle)
);

-- The dashboard's /schedules view reads the most recent rows by fired_at.
CREATE INDEX idx_scheduled_runs_recent ON scheduled_runs (fired_at DESC);
