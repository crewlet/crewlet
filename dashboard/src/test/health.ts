/**
 * A whole `health` frame for a suite to override — every member the engine
 * always sends, so a frame a test builds is one `api.Health` could have sent.
 * The members it omits when empty (`nodes`, `alarms`, `stall_lag_seconds`,
 * `unproven_seconds`, `seeded_from`) stay absent unless a case sets them.
 */

import type { EngineHealth } from "~/contract/health.ts";

export function healthFrame(over: Partial<EngineHealth> = {}): EngineHealth {
  return {
    status: "ok",
    node: "node-1",
    configured: true,
    version: "dev",
    started_at: "2026-01-01T00:00:00Z",
    queue: "memory",
    clients: 1,
    event_history_seconds: 30 * 86_400,
    spend_history_seconds: 181 * 86_400,
    in_flight: 0,
    shutting_down: false,
    posture: "serve",
    applied_epoch: 0,
    seats: [],
    ...over,
  };
}
