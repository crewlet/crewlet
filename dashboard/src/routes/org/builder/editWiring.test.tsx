/**
 * Edit org is wired to its address, only that address is the builder, and
 * the address a reader ARRIVES on says what they came to do.
 *
 * WHAT THIS PROTECTS is the join itself. Every other suite in this directory
 * mounts [Builder] directly, which is right for testing the builder and blind
 * to the one thing that has to be true for a reader to reach it: that
 * `#/agents/edit` resolves to it, that the org chart offers the way there, and
 * that no other section of Agents mounts it. A join has no type — a path is a
 * string off a URL — so a section the resolver never reaches is a feature that
 * is present in the tree, passes its own files of tests, and cannot be opened
 * by anybody. So each case goes through the ROUTE TABLE's own resolver and the
 * App's own dispatch, never a component named by hand.
 *
 * AND A LINK INTO IT IS A REQUEST. Every way into Edit org from another screen
 * asks to change one thing — a seat's "Edit", the chart's "Add seat" — so
 * `seat=` and `unit=` open that node's editor and `add=` opens the Add, ONCE,
 * on arrival (see `Builder.tsx`'s `Arrival`). "Add seat" used to land on a
 * chart with nothing to add, because nothing read `add=` at all. A link names
 * a unit by its KEY, as every link to a unit does: its name is prose two
 * teams may share.
 *
 * It is also where the read sections' apply note belongs: the builder draws a
 * DRAFT and the chart and the teams draw the projection this node has
 * APPLIED, so the note that says "still applying" must appear on those and
 * not on the builder, where it would be describing a company that is not on
 * screen.
 *
 * NOTHING HERE WAITS AGAINST A DEADLINE. The builder mounted through the App
 * runs on the browser's own clock and transport, which the App does not let a
 * suite replace, so the builder suites' `settle` cannot reach it; what is
 * waited for instead is what the builder draws once the engine has answered
 * it — its toolbar, a dialog it opens — as an event ([drawn]). Everything else
 * is either drawn on the first render (every chunk is in memory before a case
 * starts, and a chunk in memory is read synchronously — `app/lazyScreen.ts`)
 * or answered by a stubbed socket query, which [answered] runs to the end.
 */

import { answered, cleanup, fireEvent, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { Router, parseHash } from "~/app/router.tsx";
import { screenFor } from "~/app/App.tsx";
import { resolve } from "~/app/routes.ts";
import { loadChunk } from "~/app/lazyScreen.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { company, Engine, InertWebSocket } from "./testkit.tsx";
import { clearSavedChanges, recordSavedChanges } from "./savedChanges.ts";
import { waitInCase } from "./viewTestkit.tsx";

// The two chunks these addresses draw, in before a case starts, so a section
// is drawn on its first render rather than after a cold `import()`.
beforeAll(async () => {
  await Promise.all([loadChunk("agents"), loadChunk("org")]);
});

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  clearSavedChanges();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  clearSavedChanges();
  location.hash = "#/";
});

/** Whatever the App draws at `hash`, against a scripted engine. */
function mount(hash: string): void {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = hash;
  new Engine(company()).install();
  const store = new Store();
  store.applyHealth({ status: "ok" });
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = () =>
    Promise.resolve(null);
  const route = resolve(parseHash(hash).path);
  if (!route.resolved) throw new Error(`${hash} does not resolve`);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>{screenFor(route)}</Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

/** Every address in Agents that reads the org rather than editing it. */
const READS = ["#/agents", "#/agents/roster", "#/agents/teams"];

/**
 * Whether any of the builder is mounted. Its root is drawn on its FIRST
 * render, before anything has been read — unlike its toolbar, which waits for
 * the engine's answer — so its absence is a fact about which section the App
 * mounted the moment it has rendered, and not a fact about how far the
 * builder had got.
 */
const builderMounted = () => document.querySelector(".org-builder") !== null;

/** What the address's query string says, by name. */
const param = (name: string) => new URLSearchParams(location.hash.split("?")[1]).get(name);

/**
 * Waits until `find` answers with an element, and returns it.
 *
 * AN EVENT, NOT A DEADLINE: each change to the document is looked at, so it
 * resolves the moment the builder draws it, however long the engine took to
 * answer. A `findByRole` gave the builder's first load one second, which a
 * loaded runner spent before the engine's answers had been rendered.
 */
async function drawn(what: string, find: () => HTMLElement | null): Promise<HTMLElement> {
  return waitInCase<HTMLElement>(what, (done) => {
    const look = () => {
      const found = find();
      if (found) done(found);
    };
    const observer = new MutationObserver(look);
    observer.observe(document.body, { childList: true, subtree: true, attributes: true });
    look();
    return () => observer.disconnect();
  });
}

/** The builder's toolbar, once the engine has answered its reads. */
const builderDrawn = () =>
  drawn("the builder's toolbar", () =>
    screen.queryByRole("toolbar", { name: "Organization builder" }),
  );

/** The dialog named `name`, once the builder opens it. */
const dialogDrawn = (name: string) =>
  drawn(`the dialog ${name}`, () => screen.queryByRole("dialog", { name }));

/** Until nothing is open over the builder. */
const dialogsGone = () =>
  drawn("no dialog", () => (screen.queryByRole("dialog") === null ? document.body : null));

test("the org chart offers the builder, at the address the resolver gives it", async () => {
  mount("#/agents");
  await answered();
  const edit = screen.getByRole("link", { name: "Edit org" });
  expect(edit.getAttribute("href")).toBe("#/agents/edit");
  const to = resolve(parseHash(edit.getAttribute("href")!).path);
  expect(to.resolved && to.screen).toBe("org-edit");
});

test("the builder's address mounts the builder, and the read sections do not", async () => {
  mount("#/agents/edit");
  expect(builderMounted()).toBe(true);
  // Its toolbar is what the builder draws once the engine has answered, and
  // it belongs to no other section.
  await builderDrawn();
  cleanup();

  for (const hash of READS) {
    mount(hash);
    // READ AT ONCE, and that is what makes it a claim: the builder's root is
    // drawn on its first render, so a section that mounted it would show it
    // now. Its toolbar would not — that waits for the engine's answer — which
    // is why asking for the toolbar here would prove nothing.
    expect(builderMounted(), hash).toBe(false);
    cleanup();
  }
});

/*
 * A SAVE IS NOT AN APPLY. Between the two, the projection the chart and the
 * teams draw is the company before the save — a chart that has not moved,
 * which reads as a save that did nothing unless something says otherwise.
 * The builder draws the draft itself and needs no such sentence.
 */
test("the read sections say they still draw the company before the save; the builder does not", async () => {
  const settings = { revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 };
  recordSavedChanges({ settings, chart: null });
  mount("#/agents");
  await answered();
  expect(screen.getByText(/still applying settings revision/)).toBeDefined();
  cleanup();

  // The chart's writes, which this node has not applied yet.
  const chart = { position: "CREWLET_CHART_LOG@1:12", appliedHere: false };
  recordSavedChanges({ settings: null, chart });
  mount("#/agents/teams");
  await answered();
  expect(screen.getByText(/still applying the org chart's changes/)).toBeDefined();
  cleanup();

  // The control: writes this node has applied need no note.
  recordSavedChanges({ settings: null, chart: { ...chart, appliedHere: true } });
  mount("#/agents");
  await answered();
  expect(screen.queryByText(/still applying/)).toBeNull();
  cleanup();

  recordSavedChanges({ settings, chart });
  mount("#/agents/edit");
  await builderDrawn();
  expect(screen.queryByText(/still applying/)).toBeNull();
});

/*
 * A LINK THAT NAMES A SEAT OPENS ITS EDITOR — the profile's "Edit" beside a
 * seat's setup is `#/agents/edit?seat=<handle>`, and the reader who pressed it
 * is shown the form rather than a chart with one card outlined somewhere in
 * it. It waits for the builder to have read the chart, because a link names a
 * seat by the handle the chart holds it under.
 */
test("a link that names a seat opens that seat's editor", async () => {
  mount("#/agents/edit?view=table&seat=ceo");
  expect(await dialogDrawn("Edit CEO")).toBeDefined();
  // The selection is still mirrored: the link is also where the builder is.
  expect(param("seat")).toBe("ceo");
});

test("a link that names a unit by its key opens that unit's editor", async () => {
  mount("#/agents/edit?view=table&unit=engineering");
  expect(await dialogDrawn("Edit Engineering")).toBeDefined();
});

/*
 * ONCE, AND ONLY ON ARRIVAL. `seat=` is ALSO where the builder mirrors its own
 * selection, so a press on another row writes it: an editor that opened
 * whenever it changed would open on every press. Closing the one the link
 * opened leaves the reader on the builder with the seat selected and nothing
 * over it.
 */
test("the editor a link opened does not come back, and a press on a row opens none", async () => {
  mount("#/agents/edit?view=table&seat=ceo");
  fireEvent.keyDown(await dialogDrawn("Edit CEO"), { key: "Escape" });
  await dialogsGone();
  // Another row, pressed: the selection moves and the address with it.
  const designer = screen
    .getAllByRole("row")
    .find((row) => within(row).queryByText("Designer", { exact: true }))!;
  fireEvent.click(designer);
  await answered();
  expect(param("seat")).toBe("designer");
  expect(screen.queryByRole("dialog")).toBeNull();
});

/*
 * "ADD SEAT" IS AN ADD. The org chart's header links to `add=agent`, and the
 * builder opens the Add on that kind at the company's root — then takes the
 * request out of the address, so a reload or a copied link does not ask for a
 * second node. (That the header's link IS `add=agent` is `OrgChart.test.tsx`'s.)
 */
test("a link that asks for an add opens it once, on that kind, and leaves the address", async () => {
  mount("#/agents/edit?view=table&add=agent");
  const dialog = await dialogDrawn("Add to Acme");
  expect(
    within(dialog).getByRole("radio", { name: "Agent seat" }).getAttribute("aria-checked"),
  ).toBe("true");
  expect(param("add")).toBeNull();
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await dialogsGone();
  await answered();
  expect(screen.queryByRole("dialog")).toBeNull();
});

/* And with `unit=`, under that unit — the unit's own editor is not what was asked. */
test("an add with a unit opens under that unit, not the unit's editor", async () => {
  mount("#/agents/edit?view=table&unit=engineering&add=human");
  const dialog = await dialogDrawn("Add to Engineering");
  expect(
    within(dialog).getByRole("radio", { name: "Human seat" }).getAttribute("aria-checked"),
  ).toBe("true");
  expect(screen.queryByRole("dialog", { name: "Edit Engineering" })).toBeNull();
});
