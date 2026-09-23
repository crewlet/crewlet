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

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { NextFires, SchedulePeek, Schedules } from "./Schedules.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { setZone } from "~/lib/prefs.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { ScheduleRow, ScheduleRunRow, SchedulesAnswer } from "~/protocol/index.ts";

beforeEach(() => {
  // The READER's zone, which is what each instant is rendered in and is a
  // different question from the one under test. Pinned so the assertions are
  // about the schedule's zone alone.
  setZone("UTC");
});

afterEach(() => {
  cleanup();
  setZone("");
  vi.unstubAllGlobals();
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

// ---------------------------------------------------------------------------
// The list, and what it may claim about the ledger
// ---------------------------------------------------------------------------

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function fire(name: string, at: string): ScheduleRunRow {
  return {
    scope_type: "role",
    scope_id: "ceo",
    schedule_name: name,
    fire_label: at,
    target_handle: "ceo",
    scheduled_at: at,
    fired_at: at,
    outcome: "fired",
    trace_id: "",
  };
}

function mountWith(answers: Record<string, unknown>, ui = <Schedules />) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(answers[what] ?? {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{ui}</Router>
    </ClientContext.Provider>,
  );
}

const answer = (over: Partial<SchedulesAnswer>): SchedulesAnswer => ({
  schedules: [row({ name: "standup" }), row({ name: "digest" })],
  recent_runs: [],
  recent_runs_truncated: false,
  last_runs: [],
  history_available: true,
  ...over,
});

// THE LAST COLUMN IS EACH SCHEDULE'S OWN LAST FIRE. It was looked up in the
// company-wide strip, so a schedule whose fire had been pushed off it by
// busier ones read as never having fired.
test("a schedule's last fire comes from last_runs, not the company strip", async () => {
  const at = new Date(Date.now() - 3_600_000).toISOString();
  mountWith({
    schedules: answer({
      // THE STRIP HOLDS ONLY THE OTHER SCHEDULE'S FIRES.
      recent_runs: [fire("digest", at)],
      recent_runs_truncated: true,
      last_runs: [fire("digest", at), fire("standup", at)],
    }),
  });
  await waitFor(() => expect(screen.getAllByText("fired").length).toBeGreaterThanOrEqual(3));
  // The standup's own cell: two schedule rows with a fire each, plus the strip.
  expect(screen.queryByText("No fire in the ledger this node keeps")).toBeNull();
  // AND THE STRIP SAYS IT IS A PAGE OF THE LEDGER.
  expect(screen.getByText(/The newest 1 fire; there are more\./)).toBeTruthy();
});

test("an unreadable ledger is said, rather than drawn as nothing having fired", async () => {
  mountWith({ schedules: answer({ history_available: false }) });
  // ONE PER SCHEDULE ROW, AND THE STRIP'S OWN EMPTY STATE.
  await waitFor(() =>
    expect(screen.getAllByText("The dispatch ledger could not be read").length).toBe(3),
  );
  expect(screen.queryByText("No fire in the ledger this node keeps")).toBeNull();
  expect(screen.queryByText("No runs recorded")).toBeNull();
});

test("a schedule the ledger holds no fire of says only that", async () => {
  // THE CONTROL: a readable ledger with nothing in it for this schedule.
  mountWith({ schedules: answer({}) });
  await waitFor(() =>
    expect(screen.getAllByText("No fire in the ledger this node keeps").length).toBe(2),
  );
});

// WHO A FIRE REACHES IS A COUNT ON ONE LINE, AND WHOLE WHERE IT CAN WRAP. The
// cell drew three chips and a `+N` nothing linked, inside a column clipped at
// a fifth of the grid.
test("a schedule waking many seats names them all in its definition", async () => {
  const runners = ["ceo", "cto", "cfo", "pm", "swe-1", "swe-2"];
  mountWith(
    {
      schedules: answer({ schedules: [row({ name: "standup", runners })] }),
      schedule_runs: { runs: [], truncated: false },
    },
    <SchedulePeek scope="role/ceo/standup" />,
  );
  for (const handle of runners) {
    expect((await screen.findAllByText(handle)).length, handle).toBeGreaterThan(0);
  }
  expect(screen.getByText("6 seats")).toBeTruthy();
});
