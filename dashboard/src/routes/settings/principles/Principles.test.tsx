/**
 * Company › Principles as a reader meets it: the mission and the vision as two
 * cards and the standing policies numbered in a third, each under its own
 * glyph; nothing on the page but the principles; no control at all for a
 * reader who may not write them; the engine's caps as a related row only for
 * a reader who may open them; and nothing claimed about the company before
 * the chart arrives. How a part is written is the charter editor's, and its
 * own suites say so.
 */

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ScrollTextGlyph, TargetGlyph, TelescopeGlyph } from "@crewlethq/icons/glyphs";
import { Principles } from "./Principles.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";

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
  // AN OPEN LINK, so the controls are decided by the reader's grants.
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

/** Until the viewer's answer has landed, so what the page offers is decided by it. */
async function viewerAnswered() {
  await afterMicrotasks();
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
// org push lands this browser knows nothing about the company at all. The
// page's own heading says nothing about it, so it stands from the start.
test("says nothing about the company before the chart arrives", () => {
  mount();
  expect(screen.getByRole("heading", { level: 2, name: "Principles" })).toBeDefined();
  expect(screen.queryByText("No mission yet")).toBeNull();
  expect(screen.queryByText("No policies yet")).toBeNull();
  expect(screen.queryByRole("region", { name: "Mission" })).toBeNull();
  expect(screen.getByText("Loading the company's principles")).toBeDefined();
});

// THE CARDS, IN THE ORDER AN AGENT READS THEM, each a region named by its
// title one level under the page's, and the policies a numbered list with
// their count in the card's head. Mutation: draw the policies first and the
// order fails.
test("draws the mission, the vision, then the policies numbered in order", () => {
  const store = mount();
  act(() => store.applyOrg(withDerived(ACME)));
  const mission = screen.getByRole("region", { name: "Mission" });
  const vision = screen.getByRole("region", { name: "Vision" });
  const policies = screen.getByRole("region", { name: "Standing policies" });
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
  expect(policies.querySelector(".crewlet-card__header")?.textContent).toContain("2");
  for (const [first, then] of [
    [mission, vision],
    [vision, policies],
  ] as const) {
    expect(first.compareDocumentPosition(then) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  }
  // A WRITTEN PART OFFERS NO WAY TO ADD IT.
  expect(screen.queryByRole("link", { name: /^Add a/ })).toBeNull();
});

// A VISION OF PARAGRAPHS READS AS PARAGRAPHS: the engine lets the vision
// alone carry line breaks, and the card keeps them as written, in the kit's
// passage text, rather than running them into one block. Mutation: draw the
// vision as caption text and the register fails.
test("a vision of two paragraphs keeps its break, as written", () => {
  const store = mount();
  const vision = "Every team ships daily.\n\nNobody waits on a release train.";
  act(() => store.applyOrg(withDerived({ ...ACME, vision })));
  const card = screen.getByRole("region", { name: "Vision" });
  const passage = card.querySelector(".crewlet-text--passage");
  expect(passage?.tagName).toBe("P");
  expect(passage?.textContent).toBe(vision);
});

// EVERY CARD NAMES ITS PART WITH A GLYPH OF ITS OWN in its head: a target for
// what the company is for, a telescope for where it is heading, a scroll for
// the rules it wrote down. Mutation: swap two glyphs and the drawing differs.
test("each card's head carries its own glyph", () => {
  const store = mount();
  act(() => store.applyOrg(withDerived(ACME)));
  for (const [name, glyph] of [
    ["Mission", <TargetGlyph size="sm" />],
    ["Vision", <TelescopeGlyph size="sm" />],
    ["Standing policies", <ScrollTextGlyph size="sm" />],
  ] as const) {
    const icon = screen
      .getByRole("region", { name })
      .querySelector(".crewlet-card__header-icon svg");
    const drawn = document.createElement("div");
    drawn.innerHTML = renderToStaticMarkup(glyph);
    // THE SHAPES, compared as drawn: the attributes are every glyph's alike.
    expect(icon?.innerHTML, name).toBe(drawn.firstElementChild?.innerHTML);
  }
});

// NOTHING ELSE STANDS ON THE PAGE: the company's name is the shell's, its
// clock is Settings', and how agents use the page is not a principle.
test("draws no company name, no clock and no note on how agents use it", () => {
  const store = mount();
  act(() => store.applyOrg(withDerived(ACME)));
  expect(screen.queryByText("Acme")).toBeNull();
  expect(screen.queryByText(/Europe\/Lisbon/)).toBeNull();
  expect(screen.queryByText(/\d\d:\d\d/)).toBeNull();
  expect(screen.queryByText(/at the start of every turn/)).toBeNull();
  expect(screen.queryByRole("complementary")).toBeNull();
});

// AN UNWRITTEN PART SAYS SO inside its card, rather than grey text standing
// in the value's place, and an empty list claims no count.
test("each unwritten part says so in its card", () => {
  const store = mount();
  act(() => store.applyOrg(withDerived({ name: "Acme", roles: [] })));
  for (const [region, empty] of [
    ["Mission", "No mission yet"],
    ["Vision", "No vision yet"],
    ["Standing policies", "No policies yet"],
  ] as const) {
    expect(within(screen.getByRole("region", { name: region })).getByText(empty)).toBeDefined();
  }
  const policies = screen.getByRole("region", { name: "Standing policies" });
  expect(policies.querySelector(".crewlet-card__header")?.textContent).toBe("Standing policies");
});

// A READER WHO MAY WRITE THE CHARTER writes it at each card: a written part
// carries its own Edit, an unwritten one its Add, both opening the part in
// place (`edit/usePrinciplesEdit.test.tsx` walks the writing itself).
// Mutation: hand the page no editing and both fail.
test("a reader who may write the charter edits each written part at its card", async () => {
  const store = mount();
  act(() => store.applyOrg(withDerived(ACME)));
  await viewerAnswered();
  for (const [region, label] of [
    ["Mission", "Edit the mission"],
    ["Vision", "Edit the vision"],
    ["Standing policies", "Edit the standing policies"],
  ] as const) {
    const card = screen.getByRole("region", { name: region });
    expect(within(card).getByRole("button", { name: label })).toBeDefined();
    expect(within(card).queryByRole("button", { name: /^Add a/ })).toBeNull();
  }
});

test("a reader who may write the charter adds each unwritten part at its card", async () => {
  const store = mount();
  act(() => store.applyOrg(withDerived({ name: "Acme", roles: [] })));
  await viewerAnswered();
  for (const [region, add] of [
    ["Mission", "Add a mission"],
    ["Vision", "Add a vision"],
    ["Standing policies", "Add a policy"],
  ] as const) {
    const card = screen.getByRole("region", { name: region });
    expect(within(card).getByRole("button", { name: add })).toBeDefined();
    expect(within(card).queryByRole("button", { name: /^Edit/ })).toBeNull();
  }
});

// A READER WHO MAY NOT WRITE THE CHARTER reads it with no control on the page
// at all: no Edit, no Add, and no disabled copy of either standing in for it.
// Mutation: offer the admin's controls to everybody and the test fails.
test("a reader who may not write the charter reads it with no control on it", async () => {
  for (const org of [ACME, { name: "Acme", roles: [] }]) {
    const store = mount(["worklet"]);
    act(() => store.applyOrg(withDerived(org)));
    await viewerAnswered();
    for (const name of ["Mission", "Vision", "Standing policies"]) {
      const card = screen.getByRole("region", { name });
      expect(within(card).queryAllByRole("link")).toEqual([]);
      expect(within(card).queryAllByRole("button")).toEqual([]);
    }
    expect(screen.queryByRole("button", { name: /principles/i })).toBeNull();
    cleanup();
  }
});

test("the engine's caps are a related row for a reader who may open them, and nothing for one who may not", async () => {
  let store = mount(["admin"]);
  act(() => store.applyOrg(withDerived(ACME)));
  const related = await screen.findByRole("navigation", { name: "Related" });
  expect(within(related).getByRole("link").getAttribute("href")).toBe("#/settings/limits");
  cleanup();
  store = mount(["worklet"]);
  act(() => store.applyOrg(withDerived(ACME)));
  await viewerAnswered();
  expect(screen.queryByRole("navigation", { name: "Related" })).toBeNull();
});
