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
 * chart with nothing to add, because nothing read `add=` at all.
 *
 * It is also where the read screens' revision note belongs: the builder draws
 * a DRAFT and the chart and the teams draw the projection this node has
 * APPLIED, so the note that says "still applying" must appear on those and not
 * on the builder, where it would be describing a document that is not on
 * screen.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { Router, parseHash } from "~/app/router.tsx";
import { screenFor } from "~/app/App.tsx";
import { resolve } from "~/app/routes.ts";
import { loadChunk } from "~/app/lazyScreen.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { company, EDITOR, Engine, InertWebSocket } from "./testkit.tsx";
import { clearSavedRevision, recordSavedRevision } from "./savedRevision.ts";

// The two chunks these addresses draw, in before a case starts, so a case
// times what a section draws rather than a cold `import()`.
beforeAll(async () => {
  await Promise.all([loadChunk("agents"), loadChunk("org")]);
});

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  clearSavedRevision();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  clearSavedRevision();
  location.hash = "#/";
});

/** Whatever the App draws at `hash`, against a scripted configuration surface. */
function mount(hash: string) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = hash;
  new Engine(company()).install();
  const store = new Store();
  store.applyHealth({ status: "ok" });
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] });
  const socket = new LiveSocket(store);
  // The builder reads the GUARDED configuration, so the reader holds the
  // grants it is read and written under.
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "viewer" ? EDITOR : null);
  const route = resolve(parseHash(hash).path);
  if (!route.resolved) throw new Error(`${hash} does not resolve`);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>{screenFor(route)}</Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
}

/** Every address in Agents that reads the org rather than editing it. */
const READS = ["#/agents", "#/agents/roster", "#/agents/teams"];

test("the org chart offers the builder, at the address the resolver gives it", async () => {
  mount("#/agents");
  const edit = await screen.findByRole("link", { name: "Edit org" });
  expect(edit.getAttribute("href")).toBe("#/agents/edit");
  const to = resolve(parseHash(edit.getAttribute("href")!).path);
  expect(to.resolved && to.screen).toBe("org-edit");
});

test("the builder's address mounts the builder, and the read sections do not", async () => {
  mount("#/agents/edit");
  // Its toolbar is what the builder draws once the engine has answered, and
  // it belongs to no other section.
  expect(await screen.findByRole("toolbar", { name: "Organization builder" })).toBeDefined();
  cleanup();

  for (const hash of READS) {
    mount(hash);
    // Settled rather than asserted at once: the builder's toolbar appears on
    // an answer, so a check that ran before one could not tell a section that
    // does not draw it from one that has not drawn it yet.
    await waitFor(() => expect(document.body.textContent).not.toBe(""));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("toolbar", { name: "Organization builder" }), hash).toBeNull();
    cleanup();
  }
});

/*
 * A SAVE IS NOT AN APPLY. Between the two, the projection the chart and the
 * teams draw is the previous revision — a chart that has not moved, which
 * reads as a save that did nothing unless something says otherwise. The
 * builder draws the draft itself and needs no such sentence.
 */
test("the read sections say they still draw the previous revision; the builder does not", async () => {
  for (const hash of ["#/agents", "#/agents/teams"]) {
    recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
    mount(hash);
    expect(await screen.findByText(/still applying revision/), hash).toBeDefined();
    cleanup();
  }

  recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
  mount("#/agents/edit");
  await screen.findByRole("toolbar", { name: "Organization builder" });
  expect(screen.queryByText(/still applying revision/)).toBeNull();
});

/*
 * A LINK THAT NAMES A SEAT OPENS ITS EDITOR — the profile's "Edit" beside a
 * seat's setup is `#/agents/edit?seat=<handle>`, and the reader who pressed it
 * is shown the form rather than a chart with one card outlined somewhere in
 * it. It waits for the engine to key the draft, because a link names a seat by
 * the handle it RUNS under.
 */
test("a link that names a seat opens that seat's editor", async () => {
  mount("#/agents/edit?view=table&seat=ceo");
  expect(await screen.findByRole("dialog", { name: "Edit CEO" })).toBeDefined();
  // The selection is still mirrored: the link is also where the builder is.
  expect(new URLSearchParams(location.hash.split("?")[1]).get("seat")).toBe("ceo");
});

test("a link that names a unit opens that unit's editor", async () => {
  mount("#/agents/edit?view=table&unit=engineering");
  expect(await screen.findByRole("dialog", { name: "Edit Engineering" })).toBeDefined();
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
  const editor = await screen.findByRole("dialog", { name: "Edit CEO" });
  fireEvent.keyDown(editor, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  // Another row, pressed: the selection moves and the address with it.
  const designer = (await screen.findAllByRole("row")).find((row) =>
    within(row).queryByText("Designer", { exact: true }),
  )!;
  fireEvent.click(designer);
  await waitFor(() =>
    expect(new URLSearchParams(location.hash.split("?")[1]).get("seat")).toBe("designer"),
  );
  await new Promise((r) => setTimeout(r, 20));
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
  const dialog = await screen.findByRole("dialog", { name: "Add to Acme" });
  expect(
    within(dialog).getByRole("radio", { name: "Agent seat" }).getAttribute("aria-checked"),
  ).toBe("true");
  expect(new URLSearchParams(location.hash.split("?")[1]).has("add")).toBe(false);
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await new Promise((r) => setTimeout(r, 20));
  expect(screen.queryByRole("dialog")).toBeNull();
});

/* And with `unit=`, under that unit — the unit's own editor is not what was asked. */
test("an add with a unit opens under that unit, not the unit's editor", async () => {
  mount("#/agents/edit?view=table&unit=engineering&add=human");
  const dialog = await screen.findByRole("dialog", { name: "Add to Engineering" });
  expect(
    within(dialog).getByRole("radio", { name: "Human seat" }).getAttribute("aria-checked"),
  ).toBe("true");
  expect(screen.queryByRole("dialog", { name: "Edit Engineering" })).toBeNull();
});
