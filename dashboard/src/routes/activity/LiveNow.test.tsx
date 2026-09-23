/**
 * What the landing screen's two short lists say about what they leave off.
 *
 * Both are cut to a glance — the newest few events, the top few spenders — and
 * both were cut silently: the feed stopped at seven with no count beside it,
 * and "Top seats by spend" ranked six under a title that did not say seats
 * were missing, on the one card of the pair with no way to the rest. Each now
 * says how many it left off and links the screen that holds them.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { LiveNow } from "./LiveNow.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { AgentSpendRow, EventEnvelope, Rollup } from "~/protocol/index.ts";

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

const bucket = (total: number) => ({
  input_tokens: 0,
  output_tokens: 0,
  total_tokens: total,
  calls: 1,
  cost_usd: 0,
  priced_calls: 0,
});

function seatSpend(i: number): AgentSpendRow {
  return {
    ...bucket(1000 - i),
    role: `Seat ${i}`,
    handle: `seat-${i}`,
    agent_id: "",
    by_phase: {},
  };
}

function event(i: number): EventEnvelope {
  return {
    id: `e-${i}`,
    type: "task_created",
    timestamp: new Date(Date.now() - i * 1000).toISOString(),
    source: "engine",
    actor: "CEO",
    summary: `event ${i}`,
    category: "task",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: false,
  };
}

function mount({ events, seats }: { events: number; seats: number }) {
  const store = new Store();
  for (let i = events - 1; i >= 0; i--) store.applyEvent(event(i));
  const now = new Date().toISOString();
  const rollup: Rollup = {
    since: now,
    until: now,
    agent_role: "",
    totals: bucket(1),
    by_phase: [],
    by_model: [],
    by_worker: [],
    by_agent: Array.from({ length: seats }, (_, i) => seatSpend(i)),
    by_turn: [],
    turns_total: 0,
    aggregated_through: now,
  };
  store.applyTokens(rollup);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <LiveNow />
      </Router>
    </ClientContext.Provider>,
  );
}

test("the feed says how many of the held events it shows, and where the rest are", () => {
  mount({ events: 10, seats: 0 });
  expect(screen.getByText("event 6")).toBeTruthy();
  expect(screen.queryByText("event 7")).toBeNull();
  const link = screen.getByRole("link", { name: /The newest 7 of 10 events this tab holds/ });
  // THE EVENT LOG, which is `#/activity/events`: the bare `#/activity` is this
  // screen. On the whole log, since the log draws only the held events inside
  // its window and its own fallback is a day.
  expect(link.getAttribute("href")).toBe("#/activity/events?window=30d");
});

test("a feed shorter than the glance draws every event and no note", () => {
  mount({ events: 3, seats: 0 });
  expect(screen.getByText("event 2")).toBeTruthy();
  expect(screen.queryByText(/this tab holds/)).toBeNull();
});

test("the top spenders say how many seats they left off", () => {
  mount({ events: 0, seats: 8 });
  expect(screen.getByText("Seat 5")).toBeTruthy();
  expect(screen.queryByText("Seat 6")).toBeNull();
  const link = screen.getByRole("link", { name: /2 more seats spent in this window/ });
  expect(link.getAttribute("href")).toBe("#/cost");
});

test("six spenders or fewer are the whole ranking", () => {
  mount({ events: 0, seats: 6 });
  expect(screen.getByText("Seat 5")).toBeTruthy();
  expect(screen.queryByText(/more seat/)).toBeNull();
});
