/**
 * The builder is wired to its address, and only that address is the builder.
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
 * It is also where the read screens' revision note belongs: the builder draws
 * a DRAFT and the chart and the teams draw the projection this node has
 * APPLIED, so the note that says "still applying" must appear on those and not
 * on the builder, where it would be describing a document that is not on
 * screen.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { Router, parseHash } from "~/app/router.tsx";
import { screenFor } from "~/app/App.tsx";
import { resolve } from "~/app/routes.ts";
import { loadChunk } from "~/app/lazyScreen.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, storeToken } from "~/protocol/index.ts";
import { company, Engine, InertWebSocket } from "./testkit.tsx";
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
  // The builder reads the GUARDED configuration, so it needs an operator
  // token before it will ask for anything at all.
  storeToken("t");
  new Engine(company()).install();
  const store = new Store();
  store.applyHealth({ status: "ok" });
  store.applyOrg({ name: "Acme", roles: [{ name: "CEO", handle: "ceo" }], units: [] });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = () =>
    Promise.resolve(null);
  const route = resolve(parseHash(hash).path);
  if (!route.resolved) throw new Error(`${hash} does not resolve`);
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{screenFor(route)}</Router>
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
