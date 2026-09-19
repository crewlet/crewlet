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
 * It is also where the read lenses' revision note belongs: the builder draws
 * a DRAFT and the two read lenses draw the projection this node has APPLIED,
 * so the note that says "still applying" must appear on those two and not on
 * the builder, where it would be describing a document that is not on screen.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store, storeToken } from "~/protocol/index.ts";
import { CompanyScreen } from "~/routes/company/Company.tsx";
import { company, Engine, InertWebSocket } from "./testkit.tsx";
import { clearSavedRevision, recordSavedRevision } from "./savedRevision.ts";

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

/** The company screen at `hash`, against a scripted configuration surface. */
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
  return render(
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

test("the company screen offers three lenses, and the builder is one of them", () => {
  mount("#/company");
  expect(lenses()).toEqual(["Chart", "Charter", "Builder"]);
});

test("the builder lens mounts the builder, and the read lenses do not", async () => {
  mount("#/company?lens=builder");
  // Its toolbar is what the lens draws once the engine has answered, and it
  // belongs to no other lens on this screen.
  expect(await screen.findByRole("toolbar", { name: "Organization builder" })).toBeDefined();
  cleanup();

  for (const hash of ["#/company", "#/company?lens=chart", "#/company?lens=charter"]) {
    mount(hash);
    // Waited for rather than asserted at once: the builder's toolbar appears
    // on an answer, so a check that ran before one could not tell a lens that
    // does not draw it from a lens that has not drawn it yet.
    await waitFor(() => expect(screen.getByRole("radiogroup", { name: "Org view" })).toBeDefined());
    expect(screen.queryByRole("toolbar", { name: "Organization builder" }), hash).toBeNull();
    cleanup();
  }
});

/*
 * A SAVE IS NOT AN APPLY. Between the two, the projection the read lenses
 * draw is the previous revision — a chart that has not moved, which reads as
 * a save that did nothing unless something says otherwise. The builder draws
 * the draft itself and needs no such sentence.
 */
test("the read lenses say they still draw the previous revision; the builder does not", async () => {
  recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
  mount("#/company?lens=chart");
  expect(await screen.findByText(/still applying revision/)).toBeDefined();
  cleanup();

  recordSavedRevision({ revisionId: "r-saved", parentRevisionId: "r1", epoch: 4 });
  mount("#/company?lens=builder");
  await screen.findByRole("toolbar", { name: "Organization builder" });
  expect(screen.queryByText(/still applying revision/)).toBeNull();
});
