/**
 * Structure is wired to its addresses, both of its tabs are the builder, and
 * the address a reader ARRIVES on says what they came to do.
 *
 * WHAT THIS PROTECTS is the join itself. Every other suite in this directory
 * mounts [Builder] directly, which is right for testing the builder and blind
 * to the one thing that has to be true for a reader to reach it: that
 * `#/company/org` and `#/company/org/table` resolve to it, and that the org
 * pages Structure took over move a reader on to the tab holding them. A join
 * has no type (a path is a string off a URL), so a section the resolver never
 * reaches is a feature that is present in the tree, passes its own files of
 * tests, and cannot be opened by anybody. So each case goes through the ROUTE
 * TABLE's own resolver and the App's own dispatch, never a component named by
 * hand, and follows the address as the frame does.
 *
 * AND A LINK INTO IT MAY BE A REQUEST. A way into Structure from another
 * screen that asks to change one thing says so: `editor=open` beside `seat=`
 * or `unit=` opens that node's editor, and `add=` opens the Add, ONCE, on
 * arrival (see `Builder.tsx`'s `Arrival`), each then taken out of the address.
 * A selection alone is a place, not a request: it is where the builder
 * mirrors its own selection, so a reload, a Back or a seat page's crumb lands
 * on it, and it selects the node and opens nothing. The company's principles
 * are not among the requests: the company's Edit leaves for Company ›
 * Principles.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { Router, parseHash, useRoute } from "~/app/router.tsx";
import { screenFor } from "~/app/App.tsx";
import { ScreenBoundary } from "~/app/boundaries.tsx";
import { resolve, structureTabOf } from "~/app/routes.ts";
import { loadChunk } from "~/app/lazyScreen.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { company, EDITOR, Engine, InertWebSocket } from "./testkit.tsx";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import { settled } from "~/test/settle.ts";

// The chunk these addresses draw, in before a case starts, so a case times
// what a section draws rather than a cold `import()`.
beforeAll(async () => {
  await loadChunk("org");
});

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  location.hash = "#/";
});

/**
 * The screen the App draws for the address the reader is on, re-resolved on
 * every move as the frame does (`app/App.tsx`'s `Frame`), inside the routed
 * screen's boundary: a lazy screen is drawn in a boundary's region or refused
 * (`app/lazyScreen.ts`). A page a case leaves Structure for is drawn as
 * nothing: where the reader went is the address, which is what is asserted.
 */
function Routed() {
  const route = useRoute();
  const where = resolve(route.path);
  if (!where.resolved) throw new Error(`${route.hash} does not resolve`);
  if (!structureTabOf(where.screen) && where.screen !== "structure-moved") return null;
  return <ScreenBoundary resetKey={route.path.join("/")}>{screenFor(where)}</ScreenBoundary>;
}

/** Whatever the App draws at `hash`, against a scripted configuration surface. */
function mount(hash: string) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = hash;
  engine = new Engine(company());
  engine.install();
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(withDerived({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] }));
  const socket = new LiveSocket(store);
  // The builder reads the GUARDED configuration, so the reader holds the
  // grants it is read and written under.
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "viewer" ? EDITOR : null);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <Routed />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

/** The configuration surface the last [mount] scripted. */
let engine: Engine;

/**
 * Lets everything the screen asked land, and what each answer caused, with
 * nothing asked since: the point at which a dialog or a toolbar that was
 * coming would be drawn, so its absence then means something. The address
 * changes a press makes reach the router as a `hashchange`, a task of its
 * own, which the first boundary runs.
 */
const quiet = () => settled(() => engine.requests.length);

/** The query of the address the reader is on now. */
const query = () => parseHash(location.hash).query;

/** The table row whose NAME cell says `name`. */
async function tableRow(name: string): Promise<HTMLElement> {
  const rows = await screen.findAllByRole("row");
  const row = rows.find((r) =>
    [...r.querySelectorAll<HTMLElement>(".btable-name")].some(
      (cell) => within(cell).queryAllByText(name, { exact: true }).length > 0,
    ),
  );
  if (!row) throw new Error(`no table row named ${name}`);
  return row;
}

test("both of Structure's tabs are the builder, at the addresses the resolver gives them", async () => {
  for (const [hash, tab] of [
    ["#/company/org", "chart"],
    ["#/company/org/table", "table"],
  ] as const) {
    const to = resolve(parseHash(hash).path);
    expect(to.resolved && structureTabOf(to.screen), hash).toBe(tab);
    mount(hash);
    // Its toolbar is what the builder draws once the engine has answered.
    expect(
      await screen.findByRole("toolbar", { name: "Organization builder" }),
      hash,
    ).toBeDefined();
    // And the tab draws its own view of the one draft.
    if (tab === "chart") {
      expect(await screen.findByRole("tree", { name: "Structure chart" })).toBeDefined();
    } else expect(await screen.findByRole("treegrid", { name: "Organization" })).toBeDefined();
    cleanup();
  }
});

/*
 * THE PAGES STRUCTURE TOOK OVER RESOLVE, and say themselves again as the tab
 * that holds them, by replace: Edit org is the Org chart (its table view the
 * Table), the Roster and the Teams list are the Table. What their query asked
 * for is said in Structure's terms (`Structure.tsx`'s `movedTo`, whose own
 * suite holds the mapping); here it is the whole join that is held, the builder
 * drawn at the end of it.
 */
test("an earlier org page's address moves the reader on to the tab that holds it", async () => {
  for (const [hash, path] of [
    ["#/company/org/edit", "company/org"],
    ["#/company/org/edit?view=table", "company/org/table"],
    ["#/company/org/roster", "company/org/table"],
    ["#/company/org/teams", "company/org/table"],
  ] as const) {
    mount(hash);
    await waitFor(() => expect(parseHash(location.hash).path.join("/"), hash).toBe(path));
    expect(
      await screen.findByRole("toolbar", { name: "Organization builder" }),
      hash,
    ).toBeDefined();
    expect(query().has("view"), hash).toBe(false);
    cleanup();
  }
});

/*
 * AN EDIT ORG LINK THAT NAMED A SEAT STILL OPENS ITS EDITOR. It was written
 * when a selection opened one, so the move says it as the request it was:
 * the Table, the seat selected, and its editor open.
 */
test("an Edit org link naming a seat lands on the Table with that seat's editor open", async () => {
  mount("#/company/org/edit?view=table&seat=ceo");
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();
  expect(parseHash(location.hash).path.join("/")).toBe("company/org/table");
  expect(query().get("seat")).toBe("ceo");
});

/*
 * A LINK THAT ASKS FOR A SEAT'S EDITOR OPENS IT: the seat page's "Edit" beside
 * its setup is `seat=<handle>&editor=open`, and the reader who pressed it is
 * shown the form rather than a chart with one card outlined somewhere in it.
 * It waits for the engine to key the draft, because a link names a seat by
 * the handle it RUNS under. The request is then SPENT, taken out of the
 * address, while the selection stays: the address is also where the builder
 * is.
 */
test("a link that names a seat and asks for its editor opens it, and spends the request", async () => {
  mount("#/company/org/table?seat=ceo&editor=open");
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();
  await waitFor(() => expect(query().has("editor")).toBe(false));
  expect(query().get("seat")).toBe("ceo");
});

test("a link that names a unit and asks for its editor opens that unit's editor", async () => {
  mount("#/company/org/table?unit=engineering&editor=open");
  expect(await screen.findByRole("dialog", { name: "Edit Engineering" })).toBeDefined();
  await waitFor(() => expect(query().has("editor")).toBe(false));
});

/*
 * A RETURN IS NOT A REQUEST. A reload, a Back or a seat page's crumb lands on
 * the address the builder itself wrote, which names the selection and nothing
 * else: the seat is selected, on either tab, and no editor opens. Mutation:
 * honour `seat=` alone as the arrival rule did and the CEO's editor opens.
 */
test("a return to an address naming only a seat selects it and opens no editor", async () => {
  mount("#/company/org/table?seat=ceo");
  const ceo = await tableRow("CEO");
  await waitFor(() => expect(ceo.getAttribute("aria-selected")).toBe("true"));
  await quiet();
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(query().get("seat")).toBe("ceo");
  cleanup();

  mount("#/company/org?seat=ceo");
  // On the chart the toolbar offers the selection's own actions.
  expect(await screen.findByRole("button", { name: "Actions for CEO, selected" })).toBeDefined();
  await quiet();
  expect(screen.queryByRole("dialog")).toBeNull();
});

/*
 * ONCE, AND ONLY ON ARRIVAL. `seat=` is where the builder mirrors its own
 * selection, so a press on another row writes it: an editor that opened
 * whenever it changed would open on every press. Closing the one the link
 * opened leaves the reader on the builder with the seat selected and nothing
 * over it.
 */
test("the editor a link opened does not come back, and a press on a row opens none", async () => {
  mount("#/company/org/table?seat=ceo&editor=open");
  const editor = await screen.findByRole("dialog", { name: "Edit CEO" });
  fireEvent.keyDown(editor, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  // Another row, pressed: the selection moves and the address with it.
  fireEvent.click(await tableRow("Designer"));
  await waitFor(() => expect(query().get("seat")).toBe("designer"));
  await quiet();
  expect(screen.queryByRole("dialog")).toBeNull();
});

/*
 * AN ADD LINK IS AN ADD. `add=agent` opens the Add on that kind at the
 * company's root, then takes the request out of the address, so a reload or a
 * copied link does not ask for a second node.
 */
test("a link that asks for an add opens it once, on that kind, and leaves the address", async () => {
  mount("#/company/org/table?add=agent");
  const dialog = await screen.findByRole("dialog", { name: "Add to Acme" });
  expect(
    within(dialog).getByRole("radio", { name: "Agent seat" }).getAttribute("aria-checked"),
  ).toBe("true");
  expect(query().has("add")).toBe(false);
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await quiet();
  expect(screen.queryByRole("dialog")).toBeNull();
});

/*
 * THE COMPANY'S PRINCIPLES ARE THE TOOLBAR'S. Its mission, vision and standing
 * policies are written on Company › Principles and nowhere else, so its row
 * carries no Edit and Enter on it opens nothing, while the chart's toolbar,
 * with the company selected, offers Edit principles, which leaves the builder
 * for that page in edit for a reader who may write them.
 */
test("the company's row has no Edit, and the toolbar opens Company › Principles in edit", async () => {
  mount("#/company/org/table");
  const acme = await tableRow("Acme");
  expect(within(acme).queryByRole("button", { name: /^Edit|principles/ })).toBeNull();
  fireEvent.click(acme);
  fireEvent.keyDown(acme, { key: "Enter" });
  await quiet();
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(parseHash(location.hash).path.join("/")).toBe("company/org/table");
  cleanup();

  const { container } = mount("#/company/org");
  await screen.findByRole("toolbar", { name: "Organization builder" });
  fireEvent.click(
    await waitFor(() => {
      const card = container.querySelector<HTMLElement>(
        '[role="treeitem"][data-tree-id="company"]',
      );
      if (!card) throw new Error("no company card");
      return card;
    }),
  );
  fireEvent.click(await screen.findByRole("button", { name: "Actions for Acme, selected" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Edit principles/ }));
  await waitFor(() => expect(location.hash).toBe("#/company/principles?edit=mission"));
  expect(screen.queryByRole("dialog")).toBeNull();
});

/*
 * AND THE BUILDER NO LONGER OPENS THE PRINCIPLES ITSELF. An `edit=` from
 * before they moved (`edit=charter`, `edit=policies`) is a request nothing
 * here answers: the table opens as it would with none.
 */
test("an old link asking the builder for the principles opens nothing", async () => {
  mount("#/company/org/table?edit=policies");
  await screen.findAllByRole("row");
  await quiet();
  expect(screen.queryByRole("dialog")).toBeNull();
});

/* And with `unit=`, under that unit: the unit's own editor is not what was asked. */
test("an add with a unit opens under that unit, not the unit's editor", async () => {
  mount("#/company/org/table?unit=engineering&add=human");
  const dialog = await screen.findByRole("dialog", { name: "Add to Engineering" });
  expect(
    within(dialog).getByRole("radio", { name: "Human seat" }).getAttribute("aria-checked"),
  ).toBe("true");
  expect(screen.queryByRole("dialog", { name: "Edit Engineering" })).toBeNull();
});
