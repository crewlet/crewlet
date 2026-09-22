/**
 * "What each seat has learned for itself" — over EACH seat, or over twelve.
 *
 * The grid under that heading was `index.seats.filter(agent).slice(0, 12)`: a
 * bare literal at the call site, with no name, no reason and NOTHING SAYING A
 * CUT HAD HAPPENED. A company of forty agent seats was shown twelve cards
 * under a heading claiming to cover each of them, with no count and nowhere to
 * go for the rest — while the same file's container rail bounds its own peek
 * at a named `PEEK_PAGES` and links to the browse for what it leaves out.
 *
 * The bound itself is not the defect and is not removed: these seats come from
 * the org projection this screen already holds, so the grid is a peek rather
 * than a page of a read. What was missing is the marker and the way out.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Knowledge } from "./Knowledge.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/** `Knowledge.tsx`'s own PEEK_SEATS. */
const PEEK = 12;

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(agents: number) {
  const store = new Store();
  store.applyOrg({
    name: "Acme",
    roles: Array.from({ length: agents }, (_, i) => ({
      name: `Seat ${i}`,
      handle: `seat-${i}`,
    })),
  });
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = () =>
    Promise.resolve({});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Knowledge />
      </Router>
    </ClientContext.Provider>,
  );
}

test("a company with more seats than the grid draws counts the rest and links to them", async () => {
  mount(PEEK + 8);
  await waitFor(() => expect(screen.getByText("Seat 0")).toBeTruthy());
  // THE GRID STILL STOPS — the bound is the point of a peek.
  expect(screen.queryByText(`Seat ${PEEK}`)).toBeNull();
  // AND IT SAYS SO, with the number it left out and somewhere to go.
  const link = screen.getByRole("link", { name: /8 more agent seats on the roster/ });
  expect(link.getAttribute("href")).toContain("company/people");
});

test("a company the grid shows whole says nothing about a rest that does not exist", async () => {
  // THE CONTROL, and the boundary: twelve seats and twelve cards is every one
  // of them, so a "0 more" or a link here would be a caution about a company
  // with nothing missing.
  mount(PEEK);
  await waitFor(() => expect(screen.getByText(`Seat ${PEEK - 1}`)).toBeTruthy());
  expect(screen.queryByText(/more agent seat/)).toBeNull();
});
