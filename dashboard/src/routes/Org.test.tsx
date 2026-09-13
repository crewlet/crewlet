/**
 * The org screen draws the engine's hierarchy, and says so when it has none.
 *
 * The projection fixture is written out field for field from `internal/api`'s
 * `OrgProjection` with its public `derived` block, because every behaviour
 * pinned here is a reading of that block: a root seat inside the unit its
 * `unit:` reference named, a lead a unit inherits, a manager chosen by the
 * engine's rule.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { OrgScreen } from "./Org.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { DerivedSeat, OrgProjection } from "~/protocol/index.ts";

const seat = (over: Partial<DerivedSeat> & Pick<DerivedSeat, "handle" | "name">): DerivedSeat => ({
  kind: "agent",
  placed_by_ref: false,
  manager: "",
  managers: null,
  reports: null,
  auto_reports: null,
  onboarding_chain: null,
  ...over,
});

const projection: OrgProjection = {
  name: "Acme",
  mission: "Ship reliable software",
  policies: ["Write it down"],
  roles: [{ name: "CEO", handle: "ceo", manages: ["Engineering"] }, { name: "Designer" }],
  units: [
    {
      name: "Engineering",
      type: "department",
      lead: "VP Engineering",
      goals: ["Keep the lights on"],
      roles: [{ name: "VP Engineering", handle: "vpe" }],
      children: [{ name: "Backend", roles: [{ name: "Dev A" }] }],
    },
  ],
  derived: {
    seats: [
      seat({ handle: "ceo", name: "CEO", reports: ["vpe"] }),
      seat({ handle: "vpe", name: "VP Engineering", manager: "ceo", managers: ["ceo"] }),
      seat({ handle: "dev-a", name: "Dev A", manager: "vpe", managers: ["vpe"] }),
      seat({
        handle: "designer",
        name: "Designer",
        placed_by_ref: true,
        manager: "vpe",
        managers: ["vpe"],
      }),
    ],
    units: [
      {
        name: "Engineering",
        type: "department",
        lead: "vpe",
        lead_inherited: false,
        channel: "",
        channel_inherited: false,
        seats: ["vpe"],
      },
      {
        name: "Backend",
        type: "team",
        lead: "vpe",
        lead_inherited: true,
        channel: "",
        channel_inherited: false,
        seats: ["dev-a", "designer"],
      },
    ],
  },
};

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

function mount(hash: string, org: OrgProjection | null, stream: unknown = { status: "ok" }) {
  location.hash = hash;
  const store = new Store();
  if (org) store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "stream" ? stream : null);
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <OrgScreen />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, view };
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

describe("the chart", () => {
  test("a root seat placed by its unit reference sits in that unit, and says so", () => {
    const { view } = mount("#/org", projection);
    const backend = view.container.querySelector("#org-unit-u1");
    expect(backend?.textContent).toContain("Designer");
    expect(backend?.textContent).toContain("Placed by unit reference");
    // And not above every unit, where the document wrote it.
    expect(screen.getByText("Org-wide").closest("section")?.textContent).not.toContain("Designer");
  });

  test("an inherited lead is shown, and marked as inherited", () => {
    const { view } = mount("#/org", projection);
    const backend = view.container.querySelector("#org-unit-u1 .org-unit-head");
    expect(backend?.textContent).toContain("VP Engineering");
    expect(backend?.textContent).toContain("(inherited)");
    const engineering = view.container.querySelector("#org-unit-u0 .org-unit-head");
    expect(engineering?.textContent).not.toContain("(inherited)");
  });

  // A UNIT BLOCK IS ONE COMPONENT FOR ITS WHOLE LIFE. It was declared inside
  // the screen's render, a new component type every render, so each `agents`
  // push (twice per tool-loop round) tore the chart down and rebuilt it.
  test("a live push does not rebuild the tree", () => {
    const { store, view } = mount("#/org", projection);
    const before = view.container.querySelector("#org-unit-u1");
    act(() => store.applyAgents([{ role: "Dev A", state: "working" }]));
    expect(view.container.querySelector("#org-unit-u1")).toBe(before);
  });

  test("selecting a unit is a filter: it replaces the entry and rings the unit", () => {
    const { view } = mount("#/org", projection);
    const depth = history.length;
    fireEvent.click(screen.getByRole("button", { name: "Backend" }));
    expect(location.hash).toBe("#/org?unit=Backend");
    expect(history.length).toBe(depth);
    expect(view.container.querySelector("#org-unit-u1")?.classList.contains("selected")).toBe(true);
    // The same control clears it.
    fireEvent.click(screen.getByRole("button", { name: "Backend" }));
    expect(location.hash).toBe("#/org");
  });

  test("a seat named in the URL is ringed", () => {
    const { view } = mount("#/org?seat=dev-a", projection);
    const node = view.container.querySelector("#org-seat-dev-a");
    expect(node?.classList.contains("selected")).toBe(true);
    expect(node?.getAttribute("aria-current")).toBe("true");
  });

  test("an engine that sends no derived hierarchy is drawn as written, with a note", () => {
    const { derived: _omitted, ...older } = projection;
    mount("#/org", older);
    expect(screen.getByText(/did not report the hierarchy it derived/)).toBeDefined();
    // Where the document wrote it, since placement by reference is the engine's call.
    expect(screen.getByText("Org-wide").closest("section")?.textContent).toContain("Designer");
    expect(screen.queryByText("(inherited)")).toBeNull();
  });

  test("a lens this build does not know shows the chart rather than a blank", () => {
    const { view } = mount("#/org?lens=builder", projection);
    expect(view.container.querySelector("#org-unit-u0")).not.toBeNull();
  });
});

describe("with nothing to draw", () => {
  test("an engine with no active configuration offers to create the company", async () => {
    mount("#/org", null, { status: "ok", configured: false });
    expect(await screen.findByText("No organization is loaded")).toBeDefined();
    const link = await screen.findByRole("link", { name: "Create the company" });
    expect(link.getAttribute("href")).toBe("#/org?lens=builder");
  });

  test("a configured engine with an empty organization says where one comes from", async () => {
    mount("#/org", null, { status: "ok", configured: true });
    expect(await screen.findByText("No organization is loaded")).toBeDefined();
    expect(screen.queryByRole("link", { name: "Create the company" })).toBeNull();
  });
});

describe("the directory", () => {
  /** The cells of the row for one seat, by column header. */
  const rowOf = (handle: string): Record<string, HTMLElement> => {
    const headers = within(screen.getByRole("table"))
      .getAllByRole("columnheader")
      .map((h) => h.textContent ?? "");
    const row = screen.getAllByRole("row").find((r) => r.textContent?.includes(`@${handle}`));
    if (!row) throw new Error(`no row for ${handle}`);
    const cells = within(row).getAllByRole("cell");
    return Object.fromEntries(headers.map((h, i) => [h, cells[i]!]));
  };

  test("reporting lines are the engine's, and an unreported one says so", () => {
    mount("#/org?lens=directory", projection);
    // Dev A reports to the manager the engine chose, and the CEO manages the
    // one seat the engine expanded its `manages: [Engineering]` into.
    const devA = rowOf("dev-a");
    expect(within(devA["Reports to"]!).getByRole("link", { name: "VP Engineering" })).toBeDefined();
    expect(rowOf("ceo")["Manages"]!.textContent).toBe("1");
    expect(within(rowOf("ceo")["Reports to"]!).getByText("Nobody")).toBeDefined();
    cleanup();

    const { derived: _omitted, ...older } = projection;
    mount("#/org?lens=directory", older);
    // Not "Nobody" and not 0: the engine did not say.
    expect(rowOf("ceo")["Reports to"]!.textContent).toBe("Not reported");
    expect(rowOf("ceo")["Manages"]!.textContent).toBe("Not reported");
  });
});

describe("the charter", () => {
  test("mission, policies and unit goals", () => {
    mount("#/org?lens=charter", projection);
    expect(screen.getByText("Ship reliable software")).toBeDefined();
    expect(screen.getByText("Write it down")).toBeDefined();
    expect(screen.getByText("Keep the lights on")).toBeDefined();
  });
});
