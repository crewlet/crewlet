/**
 * Company › Principles as a reader meets it: the mission, the vision and the
 * standing policies numbered in order, the company's facts beside them, one
 * edit control that opens the builder at the charter, a way to write each
 * unwritten part for a reader who may, the engine's caps as a related row only
 * for a reader who may open them, and nothing claimed about the company before
 * the chart arrives.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Principles } from "./Principles.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";

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
  // THE SESSION READ IS REFUSED, so what the reader holds is the viewer's.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 404 })),
  );
  location.hash = "#/company/principles";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** The page for a reader holding `grants`; the chart is applied by the caller. */
function mount(grants: string[] = ["admin"]) {
  const store = new Store();
  // AN OPEN LINK, so the edit control is decided by the reader's grants.
  store.applyHealth(healthFrame());
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer"
      ? Promise.resolve({ login: "ana", owner: "ana", handle: "ana", grants, acts: [] })
      : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Principles />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  return store;
}

const ACME = {
  name: "Acme",
  mission: "Ship it",
  vision: "Every team ships daily",
  timezone: "Europe/Lisbon",
  policies: ["Every amount is exact to the cent.", "Ask before deleting anything."],
  roles: [],
};

// A FIGURE THE ENGINE DID NOT ANSWER IS ABSENT, never a statement: before the
// org push lands this browser knows nothing about the company at all.
test("says nothing about the company before the chart arrives", () => {
  mount();
  expect(screen.queryByText("No mission yet")).toBeNull();
  expect(screen.queryByText("No policies yet")).toBeNull();
  expect(screen.getByText("Loading the company's principles")).toBeDefined();
});

// THE STATEMENTS, IN THE ORDER AN AGENT READS THEM, each a region named by
// its label, and the policies a numbered list. Mutation: draw the policies
// first and the order fails.
test("draws the mission, the vision, then the policies numbered in order", () => {
  const store = mount();
  act(() => store.applyOrg(withDerived(ACME)));
  expect(screen.getByRole("heading", { level: 2, name: "Principles" })).toBeDefined();
  const mission = screen.getByRole("region", { name: "Mission" });
  const vision = screen.getByRole("region", { name: "Vision" });
  const policies = screen.getByRole("region", { name: "Standing policies" });
  // ONE LEVEL UNDER THE PAGE'S TITLE, every part.
  for (const name of ["Mission", "Vision", "Standing policies"]) {
    expect(screen.getByRole("heading", { level: 3, name })).toBeDefined();
  }
  expect(within(mission).getByText("Ship it")).toBeDefined();
  expect(within(vision).getByText("Every team ships daily")).toBeDefined();
  const list = within(policies).getByRole("list", { name: "Standing policies" });
  expect(list.tagName).toBe("OL");
  expect(
    within(list)
      .getAllByRole("listitem")
      .map((item) => item.textContent),
  ).toEqual(ACME.policies);
  for (const [first, then] of [
    [mission, vision],
    [vision, policies],
  ] as const) {
    expect(first.compareDocumentPosition(then) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  }
  // NO COUNT BADGE: the numbers are the count.
  expect(policies.querySelector(".crewlet-count")).toBeNull();
  // A WRITTEN PART OFFERS NO WAY TO ADD IT.
  expect(screen.queryByRole("link", { name: /^Add a/ })).toBeNull();
});

// THE FACTS ARE BESIDE THE STATEMENTS, in a landmark of their own: the name,
// the company's clock with the time it reads there now, and how agents use
// the page.
test("the company's name and clock, and how agents use the page, stand beside it", () => {
  const store = mount();
  act(() => store.applyOrg(withDerived(ACME)));
  const about = screen.getByRole("complementary", { name: "About the company" });
  expect(within(about).getByText("Acme")).toBeDefined();
  expect(within(about).getByText("Europe/Lisbon")).toBeDefined();
  expect(within(about).getByText(/^\d\d:\d\d there now$/)).toBeDefined();
  expect(within(about).getByRole("heading", { name: "How agents use this" })).toBeDefined();
  expect(within(about).getByText(/at the start of every turn/)).toBeDefined();
});

// AN UNWRITTEN PART SAYS SO, with its own way to write it that opens the
// builder in that field, rather than grey text standing in the value's place.
test("each unwritten part says so, with a way to write it at its field", async () => {
  const store = mount();
  act(() => store.applyOrg(withDerived({ name: "Acme", roles: [] })));
  expect(screen.getByText("No mission yet")).toBeDefined();
  expect(screen.getByText("No vision yet")).toBeDefined();
  expect(screen.getByText("No policies yet")).toBeDefined();
  for (const [name, part] of [
    ["Add a mission", "mission"],
    ["Add a vision", "vision"],
    ["Add a policy", "policies"],
  ]) {
    const link = await screen.findByRole("link", { name });
    expect(link.getAttribute("href")).toBe(`#/company/org/edit?edit=${part}`);
  }
});

// A READER WHO MAY NOT WRITE THE CHARTER is offered no way to: the bar says
// once why, and no disabled copy of it stands beside every unwritten part.
test("a reader who may not write the charter is offered no way to add a part", async () => {
  const store = mount(["tracker"]);
  act(() => store.applyOrg(withDerived({ name: "Acme", roles: [] })));
  await screen.findByRole("button", { name: "Edit principles" });
  expect(screen.getByText("No mission yet")).toBeDefined();
  expect(screen.queryByRole("link", { name: /^Add a/ })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Add a/ })).toBeNull();
});

// ONE EDIT CONTROL, named for the page, opening the builder at the charter,
// where the name, mission, vision and policies are all written.
test("one control edits the principles, at the builder's charter", async () => {
  const store = mount();
  act(() => store.applyOrg(withDerived({ ...ACME, policies: [] })));
  const ways = await screen.findAllByRole("link", { name: "Edit principles" });
  expect(new Set(ways.map((way) => way.getAttribute("href")))).toEqual(
    new Set(["#/company/org/edit?edit=charter"]),
  );
  expect(screen.queryByRole("link", { name: /Edit (policies|charter)/ })).toBeNull();
});

test("the engine's caps are a related row for a reader who may open them, and nothing for one who may not", async () => {
  let store = mount(["admin"]);
  act(() => store.applyOrg(withDerived(ACME)));
  const related = await screen.findByRole("navigation", { name: "Related" });
  expect(within(related).getByRole("link").getAttribute("href")).toBe("#/settings/limits");
  cleanup();
  store = mount(["tracker"]);
  act(() => store.applyOrg(withDerived(ACME)));
  // The viewer has answered once the edit control knows its access.
  await screen.findByRole("button", { name: "Edit principles" });
  expect(screen.queryByRole("navigation", { name: "Related" })).toBeNull();
});
