/**
 * The lens is wired to a screen, and only one lens is the builder.
 *
 * WHAT THIS PROTECTS is the join itself. Every other suite in this directory
 * mounts [Builder] directly, which is right for testing the builder and blind
 * to the one thing that has to be true for a reader to reach it: that the
 * COMPANY screen offers the lens, mounts it on `?lens=builder`, and mounts it
 * on nothing else. A join has no type — `lens` is a string off a URL — so a
 * lens left out of the strip, or a branch that never renders, is a feature
 * that is present in the tree, passes its own 39 files of tests, and cannot
 * be opened by anybody.
 *
 * It is also where the read lenses' apply note belongs: the builder draws a
 * DRAFT and the two read lenses draw the projection this node has APPLIED, so
 * the note that says "still applying" must appear on those two and not on the
 * builder, where it would be describing a company that is not on screen.
 *
 * NOTHING HERE WAITS AGAINST A DEADLINE. The builder mounted through this
 * screen runs on the browser's own clock and transport, which the screen does
 * not let a suite replace, so the builder suites' `settle` cannot reach it;
 * what is waited for instead is the toolbar the lens draws once the engine
 * has answered it, as an event ([builderDrawn]), and everything else on this
 * screen is either drawn on the first render or answered by a stubbed socket
 * query, which `act` runs to the end.
 */

import { answered, cleanup, render, screen, within } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { CompanyScreen } from "~/routes/company/Company.tsx";
import { company, Engine, InertWebSocket } from "./testkit.tsx";
import { clearSavedChanges, recordSavedChanges } from "./savedChanges.ts";
import { waitInCase } from "./viewTestkit.tsx";

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

/**
 * The company screen at `hash`, against a scripted engine. A case lets the
 * stubbed socket's answers land with [answered].
 */
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
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <CompanyScreen />
      </Router>
    </ClientContext.Provider>,
  );
}

/** The lens strip's options, in the order it draws them. */
const lenses = () =>
  within(screen.getByRole("radiogroup", { name: "Org view" }))
    .getAllByRole("radio")
    .map((el) => el.textContent?.trim());

/**
 * Whether any of the builder is mounted. Its root is drawn on the lens's
 * FIRST render, before anything has been read — unlike its toolbar, which
 * waits for the engine's answer — so its absence is a fact about which lens
 * the screen mounted the moment the screen has rendered, and not a fact about
 * how far a lens had got.
 */
const builderMounted = () => document.querySelector(".org-builder") !== null;

/**
 * Waits for the builder's toolbar, which the lens draws once the engine has
 * answered its reads, and holds it to its role and name.
 *
 * AN EVENT, NOT A DEADLINE: each change to the document is looked at, so it
 * resolves the moment the toolbar is drawn, however long the lens took. A
 * `findByRole` gave the lens's first load one second, which a loaded runner
 * spent before the engine's answers had been rendered.
 */
async function builderDrawn(): Promise<void> {
  const drawn = await waitInCase<HTMLElement>("the builder's toolbar", (done) => {
    const look = () => {
      const toolbar = document.querySelector<HTMLElement>(
        '[role="toolbar"][aria-label="Organization builder"]',
      );
      if (toolbar) done(toolbar);
    };
    const observer = new MutationObserver(look);
    observer.observe(document.body, { childList: true, subtree: true });
    look();
    return () => observer.disconnect();
  });
  expect(screen.getByRole("toolbar", { name: "Organization builder" })).toBe(drawn);
}

test("the company screen offers three lenses, and the builder is one of them", () => {
  mount("#/company");
  expect(lenses()).toEqual(["Chart", "Charter", "Builder"]);
});

test("the builder lens mounts the builder, and the read lenses do not", async () => {
  mount("#/company?lens=builder");
  expect(builderMounted()).toBe(true);
  // Its toolbar is what the lens draws once the engine has answered, and it
  // belongs to no other lens on this screen.
  await builderDrawn();
  cleanup();

  for (const hash of ["#/company", "#/company?lens=chart", "#/company?lens=charter"]) {
    mount(hash);
    // READ AT ONCE, and that is what makes it a claim: the builder's root is
    // drawn on its first render, so a lens that mounted it would show it now.
    // Its toolbar would not — that waits for the engine's answer — which is
    // why asking for the toolbar here proved nothing.
    expect(builderMounted(), hash).toBe(false);
    expect(lenses(), hash).toEqual(["Chart", "Charter", "Builder"]);
    cleanup();
  }
});

/*
 * A SAVE IS NOT AN APPLY. Between the two, the projection the read lenses
 * draw is the company before the save — a chart that has not moved, which
 * reads as a save that did nothing unless something says otherwise. The
 * builder draws the draft itself and needs no such sentence.
 */
test("the read lenses say they still draw the company before the save; the builder does not", async () => {
  const settings = { revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 };
  recordSavedChanges({ settings, chart: null });
  mount("#/company?lens=chart");
  await answered();
  expect(screen.getByText(/still applying settings revision/)).toBeDefined();
  cleanup();

  // The chart's writes, which this node has not applied yet.
  const chart = { position: "CREWLET_CHART_LOG@1:12", appliedHere: false };
  recordSavedChanges({ settings: null, chart });
  mount("#/company?lens=charter");
  await answered();
  expect(screen.getByText(/still applying the org chart's changes/)).toBeDefined();
  cleanup();

  // The control: writes this node has applied need no note.
  recordSavedChanges({ settings: null, chart: { ...chart, appliedHere: true } });
  mount("#/company?lens=chart");
  await answered();
  expect(lenses()).toEqual(["Chart", "Charter", "Builder"]);
  expect(screen.queryByText(/still applying/)).toBeNull();
  cleanup();

  recordSavedChanges({ settings, chart });
  mount("#/company?lens=builder");
  await builderDrawn();
  expect(screen.queryByText(/still applying/)).toBeNull();
});
