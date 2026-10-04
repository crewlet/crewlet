/**
 * Whose queue this screen asks for, and on whose authority.
 *
 * Somebody's work record — the claims on their plate, their inbox, the order
 * they mean to work in — is answered by the engine to its owner, whoever leads
 * them, and `fleet:operate`, the admin path of that rule. The screen asked for
 * it on `people:manage` instead, which is authority over person ROWS in the
 * identity directory and opens nobody's queue: an administrator holding it
 * alone was sent a refusal where the honest answer is "this is somebody
 * else's", and an operator holding `fleet:operate` alone was never shown a
 * record the engine would have given them.
 *
 * Asserted on the QUESTION, not on rows: what this screen is responsible for is
 * whether it asks at all. Each grant is held ALONE, so neither case passes on
 * the other's back.
 */

import { cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
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
  location.hash = "#/agents/seats/ceo?tab=work";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

/** What the engine answers the profile's other questions, in the wire's shapes. */
const ANSWERS: Record<string, unknown> = {
  work_items: { items: [], total_hint: 0 },
  work_inbox: { handle: "ana", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  seat_activity: { since: "", until: "", days: 7, seats: [], quantile_resolution: 0.06 },
  turns: { turns: [], next: null },
  work_my_work: {
    handle: "ceo",
    priorities: [],
    assigned: [],
    asked_of_me: [],
    checklist_items: [],
    collaborating: [],
    watching_recent: [],
    unblocked_recent: [],
    complete: true,
  },
};

/**
 * askedAs renders a colleague's seat for a reader bound to `ana` holding
 * exactly these grants, and reports every question the screen asked.
 */
async function askedAs(grants: string[]): Promise<string[]> {
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") {
      return Promise.resolve({
        login: "ana.lee",
        grants,
        handle: "ana",
        name: "Ana",
        kind: "human",
        owner: "ana",
        acts: [],
      });
    }
    return Promise.resolve(ANSWERS[what] ?? {});
  };
  store.applyOrg({ roles: [{ name: "CEO", handle: "ceo" }] });
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ViewerProvider>
          <InboxCountsProvider>
            <SeatScreen handle="ceo" />
          </InboxCountsProvider>
        </ViewerProvider>
      </Router>
    </ClientContext.Provider>,
  );
  // THE VIEWER HAS ANSWERED and the work tab has asked for its board, so
  // whatever the screen was going to ask on the viewer's authority it has.
  await waitFor(() => expect(asked).toContain("viewer"));
  await waitFor(() => expect(asked).toContain("work_items"));
  await new Promise((resolve) => setTimeout(resolve, 0));
  return asked;
}

test("people:manage alone does not ask for a colleague's queue", async () => {
  const asked = await askedAs(["state:read", "people:manage"]);
  expect(
    asked,
    "people:manage is authority over person rows, and the engine refuses a colleague's queue to it",
  ).not.toContain("work_my_work");
});

test("fleet:operate alone does", async () => {
  const asked = await askedAs(["state:read", "fleet:operate"]);
  await waitFor(() => expect(asked).toContain("work_my_work"));
});
