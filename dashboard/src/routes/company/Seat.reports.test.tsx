/**
 * The seat cards under "Direct reports", and the goal on each of them.
 *
 * The goal sat between the avatar and the state badge under `.truncate` — the
 * CELL rule, `white-space: nowrap`, a cut at whichever PIXEL came next — in
 * roughly 160px of a 300px card, with the rest of the card empty underneath. So
 * it ended mid-word: "Build and run the ingestio…". A goal is PROSE, and prose
 * is cut at a line.
 *
 * jsdom computes no layout, so what is asserted here is the half a reader can
 * check without a browser: the element is on the clamp rule and not on the cell
 * rule, it is on the card's own width rather than inside the identity row, and
 * the tail the clamp still cuts is reachable. The rule ITSELF — that `.clamp`
 * clamps and does not nowrap — is in `styles/text.test.ts`.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatPeek, SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { OrgProjection } from "~/protocol/index.ts";

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
  location.hash = "#/";
});

const GOAL =
  "Build and run the ingestion pipeline that feeds the warehouse, and keep its freshness inside the hour the SLA promises";

/** A lead with one report, and that report carries a goal longer than a line. */
const projection: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "CEO", handle: "ceo", manages: ["Engineer"] },
    { name: "Engineer", handle: "eng", goal: GOAL },
  ],
  derived: {
    units: [],
    seats: [
      {
        handle: "ceo",
        name: "CEO",
        kind: "agent",
        placed_by_ref: false,
        manager: "",
        managers: null,
        reports: ["eng"],
        auto_reports: null,
        onboarding_chain: null,
      },
      {
        handle: "eng",
        name: "Engineer",
        kind: "agent",
        placed_by_ref: false,
        manager: "ceo",
        managers: ["ceo"],
        reports: null,
        auto_reports: null,
        onboarding_chain: null,
      },
    ],
  },
};

function serving() {
  const store = new Store();
  store.applyOrg(projection);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.resolve({ llm_history: [], next: "" });
  return { store, socket };
}

function expectClamped(goal: HTMLElement, within: string) {
  expect(goal.className).toContain("clamp");
  expect(
    goal.className,
    "`.truncate` is white-space:nowrap — one line, ended at a pixel",
  ).not.toContain("truncate");
  // The part past the clamp is still reachable for a pointer, and the whole of
  // it stays in the DOM for a screen reader either way.
  expect(goal.getAttribute("title")).toBe(GOAL);
  // On the card's own width rather than squeezed between the avatar and badge.
  expect(goal.closest(".row")).toBeNull();
  expect(goal.closest(within)).not.toBeNull();
}

test("a report's goal is clamped prose on the card's own width", async () => {
  const { store, socket } = serving();
  location.hash = "#/company/people/ceo";
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
  const goal = await waitFor(() => screen.getByText(GOAL));
  expectClamped(goal, ".seat-card");
});

// THE RAIL IS THE NARROWER OF THE TWO, so the one-line cut bit harder there —
// and the peek exists precisely to answer "is this the one I meant".
test("the rail says the same thing, and it is the narrower of the two", async () => {
  const { store, socket } = serving();
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatPeek handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
  const goal = await waitFor(() => screen.getByText(GOAL));
  expectClamped(goal, ".thread-entry");
});
