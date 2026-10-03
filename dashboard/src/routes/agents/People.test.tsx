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

import { act, cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { People } from "./People.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
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
  location.hash = "#/agents/roster";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

function mount(org?: OrgProjection, agents?: unknown[]) {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  if (org) store.applyOrg(org);
  // THE ROSTER, set whole (`applySeats`; `applyAgents` only patches a seat the
  // roster already holds), each row under the agent id the store keeps it by.
  if (agents) {
    store.applySeats(
      (agents as Record<string, unknown>[]).map((a) => ({
        agent_id: `a-${String(a.handle ?? a.role ?? "")}`,
        ...a,
      })) as never,
    );
  }
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
      <ViewerProvider>
        <Router>
          <People />
        </Router>
      </ViewerProvider>
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
    location.hash = "#/agents/roster?group=flat";
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
    { id: "engineering", name: "Engineering", roles: [{ name: "Dev A" }, { name: "Dev B" }] },
    { id: "design", name: "Design", roles: [{ name: "Dee" }] },
  ],
  derived: {
    seats: [
      { handle: "dev-a", name: "Dev A", kind: "agent" },
      { handle: "dev-b", name: "Dev B", kind: "agent" },
      { handle: "dee", name: "Dee", kind: "agent" },
    ],
    units: [
      { id: "engineering", name: "Engineering", seats: ["dev-a", "dev-b"] },
      { id: "design", name: "Design", seats: ["dee"] },
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
    location.hash = "#/agents/roster?group=unit";
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
    location.hash = "#/agents/roster?group=unit&q=dev-a";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();

  expect(screen.getByText("1 seat matching")).toBeTruthy();
  expect(screen.queryByText(/directly in it/)).toBeNull();
});

// A UNIT GROUP IS ONE UNIT, NOT ONE NAME. The roster grouped by the unit's
// name, so two teams called Platform were drawn as one team of three under one
// head — and a name is prose nothing holds unique. Each group is keyed on the
// unit's key, and a name two heads share carries each one's key so a reader
// can tell them apart.
const NAMESAKES = {
  name: "Acme",
  roles: [],
  units: [
    { id: "platform", name: "Platform", roles: [{ name: "Web A" }, { name: "Web B" }] },
    { id: "infra", name: "Platform", roles: [{ name: "Metal" }] },
  ],
  derived: {
    seats: [
      { handle: "web-a", name: "Web A", kind: "agent" },
      { handle: "web-b", name: "Web B", kind: "agent" },
      { handle: "metal", name: "Metal", kind: "agent" },
    ],
    units: [
      { id: "platform", name: "Platform", seats: ["web-a", "web-b"] },
      { id: "infra", name: "Platform", seats: ["metal"] },
    ],
  },
} as unknown as OrgProjection;

test("two units sharing a name are two groups, each head naming its key", async () => {
  mount(NAMESAKES);
  await settle();
  await act(async () => {
    location.hash = "#/agents/roster?group=unit";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();

  expect(screen.getByText("Platform (platform)")).toBeTruthy();
  expect(screen.getByText("Platform (infra)")).toBeTruthy();
  expect(screen.getByText("2 seats directly in it")).toBeTruthy();
  expect(screen.getByText("1 seat directly in it")).toBeTruthy();
});

// ONE FIELD, IN THE BAR, ON EVERY SECTION. The roster left the bar's "Find a
// seat" out and drew a filter box of its own in its toolbar, so the bar
// changed shape between tabs. On a list of every seat, finding a seat IS
// narrowing the list to it: the bar's field is the roster's filter.
test("the bar's Find a seat is the roster's filter, and it draws no second field", async () => {
  mount(ORG);
  await settle();
  const fields = screen.getAllByRole("searchbox");
  expect(fields).toHaveLength(1);
  expect(fields[0]!.getAttribute("aria-label")).toBe("Find a seat");
  expect(fields[0]!.closest(".page-actions")).not.toBeNull();
  await act(async () => {
    fireEvent.change(fields[0]!, { target: { value: "dev-a" } });
  });
  await settle();
  expect(new URLSearchParams(location.hash.split("?")[1] ?? "").get("q")).toBe("dev-a");
  expect(document.querySelectorAll(".seat-card")).toHaveLength(1);
});

// ---------------------------------------------------------------------------
// The order, and the seats the chart dropped
// ---------------------------------------------------------------------------

/** The names on the cards, in the order they are drawn. */
function drawnNames(): string[] {
  return [...document.querySelectorAll(".seat-card strong")].map((el) => el.textContent ?? "");
}

// THE ORDER IS THE NAME'S, NEVER A LIVE FIELD'S. The roster this replaced
// sorted on a timestamp every push moves, so cards changed places under the
// reader's cursor while a turn ran. A push that makes Dev B the most recently
// active seat must leave the flat list alphabetical.
test("the roster is ordered by name, whatever the live rows say", async () => {
  mount(ORG, [
    {
      agent_id: "a-dev-b",
      role: "Dev B",
      handle: "dev-b",
      activity: "working",
      live_call: { updated_at: "2031-01-01T00:00:09Z" },
    },
    {
      agent_id: "a-dev-a",
      role: "Dev A",
      handle: "dev-a",
      activity: "idle",
      live_call: { updated_at: "2031-01-01T00:00:01Z" },
    },
    { agent_id: "a-dee", role: "Dee", handle: "dee", activity: "idle" },
  ]);
  await settle();
  await act(async () => {
    location.hash = "#/agents/roster?group=flat";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  });
  await settle();
  expect(drawnNames()).toEqual(["Dee", "Dev A", "Dev B"]);
});

// A SEAT THE ENGINE STILL REPORTS AND THE CHART DROPPED is drawn in its own
// group rather than vanishing: a revision that removes a seat does not stop
// the turn it was on.
test("a seat the chart no longer holds is listed as removed from the company", async () => {
  mount(ORG, [
    { agent_id: "a-dee", role: "Dee", handle: "dee", activity: "idle" },
    { agent_id: "a-old", role: "Old Seat", handle: "old-seat", activity: "working" },
  ]);
  await settle();
  expect(screen.getByText("Removed from the company")).toBeTruthy();
  expect(screen.getByText("Old Seat")).toBeTruthy();
  // PAIRED BY HANDLE: a row the chart holds is the seat's own card, never a
  // second, removed one beside it.
  expect(drawnNames().filter((n) => n === "Dee")).toHaveLength(1);
});

// A ROW IS THE CHART SEAT'S BY HANDLE, NEVER BY NAME. Paired by the role name,
// a seat the chart dropped whose name a live seat now carries was drawn as
// that seat — its state on the other's card — and listed nowhere as removed.
test("a dropped seat sharing a live seat's name is listed as removed, not as that seat", async () => {
  mount(ORG, [
    { agent_id: "a-dee", role: "Dee", handle: "dee", activity: "idle" },
    { agent_id: "a-old", role: "Dee", handle: "old-dee", activity: "working" },
  ]);
  await settle();
  expect(screen.getByText("Removed from the company")).toBeTruthy();
  expect(drawnNames().filter((n) => n === "Dee")).toHaveLength(2);
});
