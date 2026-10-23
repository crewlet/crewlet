/**
 * Company › Policies as a reader meets it: the policies numbered in order and
 * named by their card, an edit control named for what it edits that opens the
 * builder at the policies, and the engine's caps as a related row only for a
 * reader who may open them.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Policies } from "./Policies.tsx";
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
  location.hash = "#/company/policies";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** Mount the page for a reader holding `grants`, over a company with `policies`. */
function mount(grants: string[], policies: string[]) {
  const store = new Store();
  // AN OPEN LINK, so the edit control is decided by the reader's grants.
  store.applyHealth(healthFrame());
  store.applyOrg(withDerived({ name: "Acme", roles: [], policies }));
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
            <Policies />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

test("the policies are an ordered list named by their card, one item each, in order", () => {
  mount(["admin"], ["Every amount is exact to the cent.", "Ask before deleting anything."]);
  const list = screen.getByRole("list", { name: "Standing policies" });
  expect(list.tagName).toBe("OL");
  expect(
    within(list)
      .getAllByRole("listitem")
      .map((item) => item.textContent),
  ).toEqual(["Every amount is exact to the cent.", "Ask before deleting anything."]);
  // NO COUNT BADGE: the numbers are the count.
  expect(document.querySelector(".crewlet-card__header .crewlet-count")).toBeNull();
});

test("the edit control is named for the policies and opens the builder at them", async () => {
  mount(["admin"], ["Ask first."]);
  const edit = await screen.findByRole("link", { name: "Edit policies" });
  expect(edit.getAttribute("href")).toBe("#/company/org/edit?edit=policies");
  expect(screen.queryByRole("link", { name: "Edit org" })).toBeNull();
});

test("an empty list says so and offers the same way in", async () => {
  mount(["admin"], []);
  expect(screen.getByText("No policies are set")).toBeTruthy();
  const ways = await screen.findAllByRole("link", { name: "Edit policies" });
  expect(ways.map((way) => way.getAttribute("href"))).toEqual([
    "#/company/org/edit?edit=policies",
    "#/company/org/edit?edit=policies",
  ]);
});

test("the engine's caps are a related row for a reader who may open them, and nothing for one who may not", async () => {
  mount(["admin"], ["Ask first."]);
  const related = await screen.findByRole("navigation", { name: "Related" });
  expect(within(related).getByRole("link").getAttribute("href")).toBe("#/settings/limits");
  cleanup();
  mount(["tracker"], ["Ask first."]);
  await screen.findByRole("list", { name: "Standing policies" });
  // The viewer has answered once the edit control knows its access.
  await screen.findByRole("button", { name: "Edit policies" });
  expect(screen.queryByRole("navigation", { name: "Related" })).toBeNull();
});
