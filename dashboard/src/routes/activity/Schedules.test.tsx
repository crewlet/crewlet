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

import { Profiler } from "react";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { NextFires, SchedulePeek } from "./Schedules.tsx";
import { Router } from "~/app/router.tsx";
import { setZone } from "~/lib/prefs.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { ScheduleRow } from "~/protocol/index.ts";

beforeEach(() => {
  // The READER's zone, which is what each instant is rendered in and is a
  // different question from the one under test. Pinned so the assertions are
  // about the schedule's zone alone.
  setZone("UTC");
  // THE CLOCK THE PANEL READS, faked rather than handed in: the panel reads
  // the shared clock itself, so the instant under test is the system's.
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  vi.setSystemTime(NOW);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  setZone("");
});

const NOW = Date.parse("2026-06-15T00:30:00Z");

function row(over: Partial<ScheduleRow> = {}): ScheduleRow {
  return {
    scope_type: "role",
    // THE ID IS NOT THE HANDLE — a role scope is keyed on the seat's agent
    // id, and the handle rides beside it as `scope_name`. A fixture where
    // the two are equal could not tell a screen reading the id from one
    // reading the name.
    scope_id: "b9f8fba1-4fe4-522f-8349-9f28db43654f",
    scope_name: "ceo",
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
  render(<NextFires row={row()} count={3} />);
  expect(screen.getAllByText(/00:00:00/).length).toBe(3);
  expect(screen.queryByText(/09:00:00/)).toBeNull();
  expect(screen.getByText(/evaluated in Asia\/Tokyo/)).toBeTruthy();
});

// AND A ZONE-LESS ROW IS THE ENGINE'S OWN DEFAULT, not an unreadable zone:
// `nextFires` refuses to default one itself, so the default is stated here and
// stated in the subtitle, rather than yielding no fires at all.
test("a row naming no zone is worked out in UTC and says so", () => {
  render(<NextFires row={row({ timezone: "" })} count={3} />);
  expect(screen.getAllByText(/09:00:00/).length).toBe(3);
  expect(screen.getByText(/evaluated in UTC/)).toBeTruthy();
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

// ONE SCHEDULE IS NOT DRAWN ONCE A SECOND.
//
// Its page and its peek held the one-second clock for the "in 23h" of the Next
// fact and the fires panel, and drew the whole object — the header, the
// definition, the panel and every fire under it — on each tick to change words
// that move once an hour. Those read the clock themselves now, so ten seconds
// in which no word turns over commit nothing, and the words are still the
// clock's.
test("one schedule's peek draws nothing on a tick", async () => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "schedules") return Promise.resolve({ schedules: [row()] });
    if (what === "schedule_runs") return Promise.resolve({ runs: [] });
    return Promise.resolve({});
  };
  let commits = 0;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Profiler
          id="peek"
          onRender={() => {
            commits += 1;
          }}
        >
          <SchedulePeek scope={`role/${row().scope_id}/standup`} />
        </Profiler>
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
  // 00:30Z AGAINST A FIRE AT 00:00Z TOMORROW, which the Next fact and the
  // panel both read as twenty-three hours off.
  expect(screen.getAllByText("in 23h").length).toBeGreaterThan(0);

  const settled = commits;
  for (let i = 0; i < 10; i++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
  expect(commits).toBe(settled);
});
