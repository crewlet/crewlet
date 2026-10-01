/**
 * Where a seat's "Its events" leads, and by which key.
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
 * Both land on the event log, `#/activity/events`: the bare `#/activity` is
 * Live now, which reads neither filter, and is where this button used to go.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { act } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
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

/** followed opens a seat, presses "Its events" and reports where it went. */
async function followed(handle: string): Promise<URLSearchParams> {
  location.hash = `#/company/people/${handle}`;
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(
      what === "viewer"
        ? { login: "ops", grants: ["state:read", "audit:read"], kind: "machine" }
        : { count: 0, items: [] },
    );
  store.applyOrg(org);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle={handle} />
      </Router>
    </ClientContext.Provider>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  fireEvent.click(screen.getByRole("button", { name: "Its events" }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  const [path, query = ""] = location.hash.replace(/^#/, "").split("?");
  expect(path, "the event log, never Live now").toBe("/activity/events");
  return new URLSearchParams(query);
}

test("a person's seat opens the events that name its handle as the actor", async () => {
  const params = await followed("ada");
  expect(params.get("actor"), "the handle every write they make is recorded under").toBe("ada");
  expect(params.get("seat"), "a person has no agent id to narrow by").toBeNull();
});

test("an agent's seat opens the events of its agent id", async () => {
  const params = await followed("ceo");
  expect(params.get("seat")).toBe("ceo");
  expect(params.get("actor"), "a name is shared by namesakes").toBeNull();
});
