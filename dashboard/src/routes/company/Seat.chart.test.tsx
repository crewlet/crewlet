/**
 * Where a seat sits in the chart, and the one line under the number that says
 * it.
 *
 * The "Direct reports" tile counted who reports to THIS seat and captioned it
 * "reports to <manager>" — the opposite relation, in the line a reader takes as
 * the number's own footnote, and a duplicate of a fact the header above and the
 * "Who this is" rail below both already state. It also drew a measured `0` where
 * the engine had sent no hierarchy at all, which is the one absence every
 * neighbouring row on this screen already tells apart.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatPeek, SeatScreen } from "./Seat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { DerivedSeat, OrgProjection } from "~/protocol/index.ts";

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

const derived = (over: Partial<DerivedSeat> & { handle: string; name: string }): DerivedSeat => ({
  kind: "agent" as const,
  placed_by_ref: false,
  manager: "",
  managers: null,
  reports: null,
  auto_reports: null,
  onboarding_chain: null,
  ...over,
});

/**
 * A chart where the VP leads a team it does not sit in, so its reports are
 * automatic, and the CEO's three are all from its own `manages` list. Those are
 * the two halves the caption has to tell apart.
 */
const projection: OrgProjection = {
  name: "Acme",
  roles: [
    { name: "CEO", handle: "ceo", manages: ["VP Eng", "Dev A", "Dev B"] },
    { name: "VP Eng", handle: "vpe" },
  ],
  units: [
    {
      name: "Backend",
      type: "team",
      lead: "VP Eng",
      roles: [
        { name: "Dev A", handle: "dev-a" },
        { name: "Dev B", handle: "dev-b" },
      ],
    },
  ],
  derived: {
    seats: [
      derived({ handle: "ceo", name: "CEO", reports: ["vpe", "dev-a", "dev-b"] }),
      derived({
        handle: "vpe",
        name: "VP Eng",
        manager: "ceo",
        managers: ["ceo"],
        reports: ["dev-a", "dev-b"],
        auto_reports: ["dev-a", "dev-b"],
      }),
      derived({ handle: "dev-a", name: "Dev A", manager: "ceo", managers: ["ceo", "vpe"] }),
      derived({ handle: "dev-b", name: "Dev B", manager: "ceo", managers: ["ceo", "vpe"] }),
    ],
    units: [
      {
        name: "Backend",
        type: "team",
        // A HANDLE: the derived block's own vocabulary throughout, which is why
        // the overlay resolves it through `byHandle` and refuses a name.
        lead: "vpe",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["dev-a", "dev-b"],
      },
    ],
  },
};

function mount(handle: string, org: OrgProjection = projection) {
  const store = new Store();
  store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.resolve({ llm_history: [], next: "" });
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatScreen handle={handle} />
      </Router>
    </ClientContext.Provider>,
  );
}

/** The stat tile carrying this label, as a reader meets it. */
function tile(label: string): HTMLElement {
  const found = screen
    .getAllByText(label)
    .map((el) => el.closest<HTMLElement>(".crewlet-statcard"))
    .find((el): el is HTMLElement => el !== null);
  expect(found, `no stat tile labelled ${label}`).toBeTruthy();
  return found!;
}

const sub = (t: HTMLElement) => t.querySelector(".crewlet-statcard__sub")?.textContent;
const value = (t: HTMLElement) => t.querySelector(".crewlet-statcard__value")?.textContent;

test("the direct-reports caption breaks that number down, and never names the manager", async () => {
  mount("ceo");
  await waitFor(() => expect(screen.getAllByText("Direct reports").length).toBeGreaterThan(0));
  const t = tile("Direct reports");
  expect(value(t)).toBe("3");
  expect(sub(t)).toBe("all from its manages list");
  // The regression itself: the manager was named under a count of who this seat
  // manages, which is the opposite relation.
  expect(t.textContent).not.toContain("reports to");
});

// A SEAT THAT LEADS A UNIT MANAGES ITS MEMBERS WITHOUT ANYBODY WRITING IT DOWN,
// which is the question a surprising count actually raises. `auto_reports` is
// the engine's own record of it and nothing in this client read it.
test("reports that arrived by leading a unit say so", async () => {
  mount("vpe");
  await waitFor(() => expect(screen.getAllByText("Direct reports").length).toBeGreaterThan(0));
  expect(sub(tile("Direct reports"))).toBe("all by leading a unit");
});

test("a seat nobody reports to says that, rather than naming who it reports to", async () => {
  mount("dev-a");
  await waitFor(() => expect(screen.getAllByText("Direct reports").length).toBeGreaterThan(0));
  const t = tile("Direct reports");
  expect(value(t)).toBe("0");
  expect(sub(t)).toBe("nobody reports to this seat");
});

// AND NO DERIVED BLOCK IS NOT A COUNT OF ZERO. `reports` is empty because
// nothing was said, not because nobody reports here, and every neighbouring row
// on this screen already draws that difference.
test("no derived block is not a count of zero", async () => {
  const { derived: _drop, ...flat } = projection;
  mount("ceo", flat as OrgProjection);
  await waitFor(() => expect(screen.getAllByText("Direct reports").length).toBeGreaterThan(0));
  const t = tile("Direct reports");
  expect(value(t)).not.toBe("0");
  expect(t.textContent).toContain("Not reported by this engine");
  expect(sub(t)).toBe("this engine did not report its hierarchy");
});

// AND THE RAIL AGREES WITH THE HEADER THREE INCHES ABOVE IT, which already read
// "not reported by this engine" out of the same flag.
test("the peek does not claim nobody reports to a seat the engine said nothing about", async () => {
  const { derived: _drop, ...flat } = projection;
  const store = new Store();
  store.applyOrg(flat as OrgProjection);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <SeatPeek handle="ceo" />
      </Router>
    </ClientContext.Provider>,
  );
  await waitFor(() => expect(screen.getByText("Direct reports")).toBeTruthy());
  expect(screen.queryByText(/Nobody reports to this seat/)).toBeNull();
  expect(screen.getByText(/did not report its hierarchy/)).toBeTruthy();
});
