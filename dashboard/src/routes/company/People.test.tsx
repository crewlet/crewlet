/**
 * One keystroke moves one parameter.
 *
 * `useTab`'s third argument decides whether a parameter takes the number keys:
 * a SECTION is the page you are on and binds them, a FILTER narrows what is on
 * it and leaves them alone (`app/frame/tabs.ts`, and the hook's own case for
 * it in `tabs.test.tsx`). The hook has always been right about this. This
 * screen was not.
 *
 * People declares two parameters — which view, and how the roster is grouped —
 * and BOTH were sections. `useKeyChords` installs one window listener per
 * call and each listener returns after its OWN first match, so there is no
 * precedence between them: the two both fired. Pressing `1` set the view to
 * `seats` and the grouping to `state`; pressing `2` set the view to `workload`
 * and the grouping to `unit`. A reader reaching for a view silently regrouped
 * the table under it, and a reader reaching past the end of one set hit the
 * other.
 *
 * It also put every regroup in the history, so Back walked through groupings
 * instead of leaving the screen.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { People } from "./People.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";

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
  location.hash = "#/company/people";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

function mount(org?: OrgProjection) {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  if (org) store.applyOrg(org);
  const socket = new LiveSocket(store);
  (
    socket as unknown as {
      query: (what: string, params?: Record<string, unknown>) => Promise<unknown>;
    }
  ).query = (what: string) => {
    if (what === "work_workload") return Promise.resolve({ rows: [] });
    return Promise.resolve({});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <People />
      </Router>
    </ClientContext.Provider>,
  );
}

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

/** What the URL says the two parameters are, defaults included. */
function params() {
  const query = new URLSearchParams(location.hash.split("?")[1] ?? "");
  return { view: query.get("view") ?? "seats", group: query.get("group") ?? "state" };
}

// THE REPORTED SHAPE: reach for the second view, land there AND regroup.
test("a digit moves the view and leaves the grouping alone", async () => {
  mount();
  await settle();

  // Start from a grouping that is not the default, so a listener that fires
  // has something visible to overwrite — asserting against the fallback would
  // pass whether or not the second setter ran.
  await act(async () => {
    location.hash = "#/company/people?group=flat";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();
  expect(params().group).toBe("flat");

  await act(async () => {
    fireEvent.keyDown(window, { key: "2" });
  });
  await settle();

  expect(params().view).toBe("workload");
  // The grouping is the reader's, and nothing they pressed was about it.
  expect(params().group).toBe("flat");
});

// AND A DIGIT PAST THE VIEWS IS NOT THE GROUPINGS'. There are two views and
// three groupings, so `3` had exactly one binding — the grouping's — and a
// reader stepping off the end of the strip changed something else entirely.
test("a digit past the end of the views changes nothing", async () => {
  mount();
  await settle();

  await act(async () => {
    fireEvent.keyDown(window, { key: "3" });
  });
  await settle();

  expect(params()).toEqual({ view: "seats", group: "state" });
});

// ---------------------------------------------------------------------------
// What a group head's number counts
// ---------------------------------------------------------------------------

/** Two units, one with two seats and one with one. */
const ORG = {
  name: "Acme",
  roles: [],
  units: [
    { name: "Engineering", roles: [{ name: "Dev A" }, { name: "Dev B" }] },
    { name: "Design", roles: [{ name: "Dee" }] },
  ],
  derived: {
    seats: [
      { handle: "dev-a", name: "Dev A", kind: "agent" },
      { handle: "dev-b", name: "Dev B", kind: "agent" },
      { handle: "dee", name: "Dee", kind: "agent" },
    ],
    units: [
      { name: "Engineering", seats: ["dev-a", "dev-b"] },
      { name: "Design", seats: ["dee"] },
    ],
  },
} as unknown as OrgProjection;

// A BARE NUMBER BESIDE A UNIT'S NAME IS THE THIRD FIGURE THE SAME UNIT
// CARRIED. The workspace rail drew its whole subtree, the org chart's block
// drew its own members, and this drew `rows.length` — none of them saying
// which. A roster group can only ever be the direct members, because a seat
// sits in exactly one group.
test("a unit group head says the number is the unit's own members", async () => {
  mount(ORG);
  await settle();
  await act(async () => {
    location.hash = "#/company/people?group=unit";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();

  expect(screen.getByText("2 seats directly in it")).toBeTruthy();
  expect(screen.getByText("1 seat directly in it")).toBeTruthy();
});

// AND A FILTER OUTRANKS IT. With one on, every count on the screen is over
// what MATCHED — "2 seats directly in it" above a single card is the same lie
// in the other direction.
test("a filtered group head counts what matched, not what the unit holds", async () => {
  mount(ORG);
  await settle();
  await act(async () => {
    location.hash = "#/company/people?group=unit&q=dev-a";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();

  expect(screen.getByText("1 seat matching")).toBeTruthy();
  expect(screen.queryByText(/directly in it/)).toBeNull();
});
