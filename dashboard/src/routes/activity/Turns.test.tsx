/**
 * The turns list draws the rows that changed, and no others.
 *
 * Two hundred turns, polled every twenty seconds, on a screen that renders
 * whenever a seat's live state moves. Measured under the development build, a
 * poll that brought back the same two hundred turns drew all of them (179–220
 * ms), a poll that changed one drew all of them, and a seat moving a round
 * drew all of them — the first because every answer arrived as new objects,
 * the other two because the screen built its column list inline. These cases
 * count the rows the screen's own cells draw ([countingGrid]).
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

vi.mock("~/app/frame/DataGrid.tsx", async (importOriginal) => {
  const { countingGrid } = await import("~/test/rowsDrawn.tsx");
  return countingGrid(importOriginal);
});

import { drawnRows } from "~/test/rowsDrawn.tsx";
import { Turns } from "./Turns.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { TurnRow } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** The poll the screen asks again on — `Turns.tsx`'s own. */
const POLL_MS = 20_000;
const NOW = Date.parse("2026-09-30T12:00:00Z");

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "Date"] });
  vi.setSystemTime(NOW);
  location.hash = "#/activity/turns";
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

function turn(i: number, summary = `summary ${i}`): TurnRow {
  return {
    turn_id: `t-${i}`,
    work_key: `w-${i}`,
    agent_id: "a-1",
    role: "Ada",
    started_at: new Date(NOW - (i + 1) * 60_000).toISOString(),
    ended_at: new Date(NOW - (i + 1) * 60_000 + 5_000).toISOString(),
    duration_ms: 5_000,
    complete: true,
    phases: 2,
    iterations: 1,
    failed: false,
    input_tokens: 10,
    output_tokens: 10,
    total_tokens: 20,
    summary,
  };
}

/** What the engine answers now — handed over as a fresh parse, as the wire does. */
let answer: TurnRow[] = [];

async function mount() {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = async () =>
    JSON.parse(JSON.stringify({ turns: answer })) as unknown;
  store.applySeats([{ id: "ada", agent_id: "a-1", name: "Ada", role: "Ada", kind: "agent" }]);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Turns />
      </Router>
    </ClientContext.Provider>,
  );
  await poll(0);
  return store;
}

async function poll(ms = POLL_MS) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

test("a poll whose answer is unchanged draws no turn", async () => {
  answer = Array.from({ length: 50 }, (_, i) => turn(i));
  await mount();
  expect(drawnRows()).toHaveLength(50);
  await poll();
  expect(drawnRows()).toEqual([]);
});

test("a poll that changed one turn draws that turn and no other", async () => {
  answer = Array.from({ length: 50 }, (_, i) => turn(i));
  await mount();
  drawnRows();
  answer = answer.map((t, i) => (i === 7 ? turn(7, "changed") : t));
  await poll();
  expect(drawnRows()).toEqual(["t-7"]);
});

// THE FEED IS NEWEST FIRST: a turn that starts moves every other one down a
// place, and only the new one has anything to draw.
test("a poll that brought a new turn draws that turn and no other", async () => {
  answer = Array.from({ length: 50 }, (_, i) => turn(i + 1));
  await mount();
  drawnRows();
  answer = [turn(0, "new"), ...answer];
  await poll();
  expect(drawnRows()).toEqual(["t-0"]);
});

// AND THE SCREEN'S OWN RENDERS DRAW NOTHING. It reads the seats' live state for
// its filter chips, so a seat moving a round renders it — which, with the
// columns built inline, drew every turn.
test("a seat's live state moving draws no turn", async () => {
  answer = Array.from({ length: 50 }, (_, i) => turn(i));
  const store = await mount();
  drawnRows();
  act(() => {
    store.applyAgents([{ agent_id: "a-1", state: "running" }]);
  });
  expect(drawnRows()).toEqual([]);
});

// AND THE COLUMNS STILL MOVE WITH WHAT THEY READ. The re-run marker is a fact
// about every row sharing a trigger, so a new run of an old trigger has to
// redraw the old row too: a column list held still on nothing would leave it
// unmarked while its new run said "re-run" beside it.
test("a turn that re-runs a trigger marks the earlier run as well", async () => {
  answer = Array.from({ length: 5 }, (_, i) => turn(i));
  await mount();
  drawnRows();
  answer = [{ ...turn(9, "again"), work_key: "w-3" }, ...answer];
  await poll();
  const drawn = drawnRows();
  expect(drawn).toContain("t-9");
  expect(drawn).toContain("t-3");
  expect(screen.getAllByText("re-run")).toHaveLength(2);
});
