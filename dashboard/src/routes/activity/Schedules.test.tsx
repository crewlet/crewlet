/**
 * Which zone the fires after the next one are worked out in.
 *
 * The engine resolves a schedule's own timezone and evaluates the expression
 * there (`internal/schedule/entries.go`, `Expr.FireTimes`), so a reading aid
 * that worked the same expression out in UTC did not merely drift across a
 * daylight-saving change — it was wrong by the zone's STANDING offset on every
 * row of every company that does not run in UTC. This panel is the only place
 * in the dashboard that predicts a fire rather than reporting the one the
 * engine already computed, so it is the only place that can be wrong this way.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { NextFires } from "./Schedules.tsx";
import { setZone } from "~/lib/prefs.ts";
import type { ScheduleRow } from "~/protocol/index.ts";

beforeEach(() => {
  // The READER's zone, which is what each instant is rendered in and is a
  // different question from the one under test. Pinned so the assertions are
  // about the schedule's zone alone.
  setZone("UTC");
});

afterEach(() => {
  cleanup();
  setZone("");
});

const NOW = Date.parse("2026-06-15T00:30:00Z");

function row(over: Partial<ScheduleRow> = {}): ScheduleRow {
  return {
    scope_type: "role",
    scope_id: "ceo",
    name: "standup",
    cron: "0 9 * * *",
    timezone: "Asia/Tokyo",
    task: "run the standup",
    target: "",
    enabled: true,
    timeout_seconds: 600,
    catchup: false,
    runners: ["ceo"],
    next_run: "2026-06-16T00:00:00Z",
    ...over,
  };
}

// 09:00 IN TOKYO IS 00:00Z, and the nine-hour gap is the whole finding: the
// list used to be worked out in UTC and put every fire nine hours late, for
// ever, on a screen whose header carries the engine's own answer right above
// it.
test("the fires are worked out in the schedule's zone, not in UTC", () => {
  render(<NextFires row={row()} now={NOW} count={3} />);
  expect(screen.getAllByText(/00:00:00/).length).toBe(3);
  expect(screen.queryByText(/09:00:00/)).toBeNull();
  expect(screen.getByText(/evaluated in Asia\/Tokyo/)).toBeTruthy();
});

// AND A ZONE-LESS ROW IS THE ENGINE'S OWN DEFAULT, not an unreadable zone:
// `nextFires` refuses to default one itself, so the default is stated here and
// stated in the subtitle, rather than yielding no fires at all.
test("a row naming no zone is worked out in UTC and says so", () => {
  render(<NextFires row={row({ timezone: "" })} now={NOW} count={3} />);
  expect(screen.getAllByText(/09:00:00/).length).toBe(3);
  expect(screen.getByText(/evaluated in UTC/)).toBeTruthy();
});
