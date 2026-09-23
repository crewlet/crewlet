/**
 * What the seat screen says a token figure covers.
 *
 * A seat wears two token tiles, one per tab, and they are fed from two
 * different places on purpose: Overview reads the rollup the projection PUSHES
 * — always in hand, which is why that tab fires no query for it — and the Cost
 * tab asks a windowed `tokens` query of its own. The pushed rollup's window is
 * the engine's (`livestate.LiveSpendWindow`), not the client's to name, so the
 * tile has to say what the answer says it covers.
 *
 * Labelled with a literal instead, the two tiles carried the SAME heading over
 * numbers that differ by a factor of several — a day's spend under a week's
 * word, one tab click apart — and a founder comparing seats on the roster was
 * reading the wrong one. The engine's own rollup carries `since`/`until`
 * because "a figure labelled with the wrong window is worse than an unlabelled
 * one"; this is the client half of that.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { Bucket, Rollup } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function bucket(over: Partial<Bucket> = {}): Bucket {
  return {
    input_tokens: 4_000,
    output_tokens: 1_000,
    total_tokens: 5_000,
    calls: 12,
    ...over,
  };
}

/** The rollup as the projection pushes it: one window, stated on the answer. */
function rollup(since: string, until: string): Rollup {
  return {
    since,
    until,
    agent_role: "",
    totals: bucket(),
    by_phase: [],
    by_model: [],
    by_worker: [],
    by_agent: [{ ...bucket(), role: "CEO", handle: "ceo", agent_id: "a-1", by_phase: {} }],
    by_turn: [],
    aggregated_through: until,
  };
}

function mount(pushed: Rollup) {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = () =>
    Promise.resolve({});
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  store.applyTokens(pushed);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
}

// THE TILE NAMES THE ROLLUP'S OWN WINDOW.
test("the overview token tile is headed with the window the rollup reports", () => {
  mount(rollup("2026-09-12T10:00:00Z", "2026-09-13T10:00:00Z"));
  expect(screen.getByText("Tokens · 24 hours")).toBeTruthy();
  // AND NOT A WINDOW NOBODY MEASURED. "7d" is the Cost tab's query, and this
  // tab has not asked it — a heading borrowed from the other tile is a claim
  // about six days this number does not cover.
  expect(screen.queryByText("Tokens · 7d")).toBeNull();
});

// AND IT FOLLOWS THE ANSWER RATHER THAN A CONSTANT.
//
// `LiveSpendWindow` is the engine's to change, and the whole reason the rollup
// states its own edges. A second literal here — "24h" — would be correct today
// and wrong silently on the day it moves, which is exactly how the "7d" it
// replaces came to be wrong.
test("a rollup over a different window is headed with that window", () => {
  mount(rollup("2026-09-13T04:00:00Z", "2026-09-13T10:00:00Z"));
  expect(screen.getByText("Tokens · 6 hours")).toBeTruthy();
  expect(screen.queryByText("Tokens · 24 hours")).toBeNull();
});
