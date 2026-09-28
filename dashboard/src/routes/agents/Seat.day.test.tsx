/**
 * A person's day on their seat page, and the one word it must not reuse.
 *
 * "Queue" is the open work ASSIGNED to a person — the Queue tab on My work and
 * the sidebar's figure are that count. The tile here counts the person's
 * PRIORITIES, a list somebody wrote that can name a colleague's task, and it
 * was labelled "Queue" too: "Queue 5" on the seat page beside "Queue 2" on My
 * work, one word naming two numbers.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { ViewerProvider } from "~/lib/viewer.ts";

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
  location.hash = "#/agents/seats/jane";
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

test("a person's priorities are counted as Priorities, never as their Queue", async () => {
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    if (what === "viewer") {
      return Promise.resolve({ operator_id: "jane-token", handle: "jane", acts: [] });
    }
    if (what === "work_person") {
      return Promise.resolve({
        handle: "jane",
        held: true,
        priorities: ["t1", "t2", "t3", "t4", "t5"],
        unread: [],
        pinned_views: [],
      });
    }
    return Promise.resolve({});
  };
  store.applyOrg({ roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }] });
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <SeatScreen handle="jane" />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  // THE PERSON'S OWN RECORD, read once the viewer is known to be them.
  await waitFor(() => expect(screen.getByText("5")).toBeTruthy());
  expect(screen.getByText("Priorities")).toBeTruthy();
  expect(screen.queryByText("Queue")).toBeNull();
});
