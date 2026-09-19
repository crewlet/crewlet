/**
 * What "Assigned and open" is a list of.
 *
 * The tracker's query grammar defaults to `subtasks=collapsed`, where a filter
 * is a predicate on the ROOT task and its whole subtree rides along
 * UNFILTERED. That is right for the board, which draws a tree, and wrong for
 * this card: its heading says assigned, its count is the number of rows, and
 * its empty state reads "Nothing open is assigned to them".
 *
 * Measured against a running engine, @agent-cto's card said 9 and listed six
 * tasks assigned to Backend Engineer — the subtasks of an epic the CTO owns.
 * The same six did not appear under Backend Engineer's own card, because
 * THEIR roots are not assigned to them, so the two seats' pages disagreed
 * about who was holding the same six tasks.
 *
 * Asserted on the QUESTION rather than on the rows: the bleed happens in the
 * engine's SQL, so a fixture answering this query can be given any rows at all
 * and the card renders them. What this screen is responsible for is asking for
 * every row to be filtered on its own — the same thing `Work.tsx` already
 * asks for, one screen over.
 */

import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

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
  location.hash = "#/company/people/ceo?tab=work";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

test("the seat's work list asks for every row to be filtered on its own", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: [string, Record<string, unknown>?][] = [];
  (
    socket as unknown as { query: (what: string, p?: Record<string, unknown>) => Promise<unknown> }
  ).query = (what, p) => {
    asked.push([what, p]);
    return Promise.resolve({ count: 0, items: [] });
  };
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );

  await waitFor(() => expect(asked.some(([what]) => what === "work_items")).toBe(true));
  const params = asked.findLast(([what]) => what === "work_items")?.[1] ?? {};
  expect(params.assignee).toBe("ceo");
  expect(
    params.subtasks,
    "without it the filter is a predicate on the root and a subtree rides along unfiltered",
  ).toBe("separate");
});
