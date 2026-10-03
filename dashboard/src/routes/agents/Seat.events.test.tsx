/**
 * Where a seat's "Events" leads, and by which key.
 *
 * The activity feed matches `actor` for EQUALITY against the name each event
 * records as having acted, and what the engine records for a person bound to a
 * seat is that seat's HANDLE — `iam.ActorFor` names the seat as the author of
 * every write they make, and the identity trail's `by` carries the same value.
 * The link asked for the seat's display NAME instead, which no event carries,
 * so a person's seat opened an empty feed that read as somebody who had never
 * done anything — and, the day two seats shared a name, would have listed a
 * namesake's events as theirs.
 *
 * An agent seat has an agent id, and the feed narrows by that through `seat=`.
 *
 * Both land on the event log, `#/live/events`: the bare `#/live` is Live now,
 * which reads neither filter. The action is the profile's "More" menu's, for
 * a person as for an agent — it was an agent's only, on the claim that a
 * person publishes nothing a filter could find, which the actor match answers.
 */

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
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

function derived(handle: string, name: string, kind: "agent" | "human") {
  return {
    handle,
    name,
    kind,
    placed_by_ref: false,
    manager: "",
    managers: null,
    reports: null,
    auto_reports: null,
    onboarding_chain: null,
  };
}

// A NAME THAT IS NOT THE HANDLE, which is the ordinary case — a person's seat
// is titled with who they are and addressed by a slug — and the only one in
// which the two keys can be told apart.
const org: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "Chief Executive", handle: "ceo" },
    { name: "Ada Founder", handle: "ada", kind: "human" },
  ],
  units: [],
  derived: {
    seats: [derived("ceo", "Chief Executive", "agent"), derived("ada", "Ada Founder", "human")],
    units: [],
  },
};

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  location.hash = "";
});

/** What the engine answers the profile's other questions, in the wire's shapes. */
const ANSWERS: Record<string, unknown> = {
  viewer: { login: "ops.person", grants: ["state:read", "audit:read"], handle: "", acts: [] },
  work_items: { items: [], total_hint: 0 },
  work_inbox: { handle: "", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  seat_activity: { since: "", until: "", days: 7, seats: [], quantile_resolution: 0.06 },
  turns: { turns: [], next: null },
};

/** followed opens a seat, presses "Events" in its More menu and reports where it went. */
async function followed(handle: string, name: string): Promise<URLSearchParams> {
  location.hash = `#/agents/seats/${handle}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(ANSWERS[what] ?? {});
  store.applyOrg(org);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ViewerProvider>
          <InboxCountsProvider>
            <SeatScreen handle={handle} />
          </InboxCountsProvider>
        </ViewerProvider>
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  fireEvent.click(screen.getByRole("button", { name: `More on ${name}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: /^Events/ }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  const [path, query = ""] = location.hash.replace(/^#/, "").split("?");
  expect(path, "the event log, never Live now").toBe("/live/events");
  return new URLSearchParams(query);
}

test("a person's seat opens the events that name its handle as the actor", async () => {
  const params = await followed("ada", "Ada Founder");
  expect(params.get("actor"), "the handle every write they make is recorded under").toBe("ada");
  expect(params.get("seat"), "a person has no agent id to narrow by").toBeNull();
});

test("an agent's seat opens the events of its agent id", async () => {
  const params = await followed("ceo", "Chief Executive");
  expect(params.get("seat")).toBe("ceo");
  expect(params.get("actor"), "a name is shared by namesakes").toBeNull();
});
