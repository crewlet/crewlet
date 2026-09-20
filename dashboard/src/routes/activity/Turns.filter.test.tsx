/**
 * The seat filter on the turns screen, and what it hides.
 *
 * The chip row is a PICKER: every seat it draws is one a reader can narrow
 * to, and every seat it does not draw is one they cannot. It opened with
 * eight of them and said nothing after the eighth, so on a company with more
 * agent seats than that the rest were reachable only by typing `?role=` into
 * the address bar — a filter nobody finds, on a screen whose whole job is
 * narrowing. A picker showing part of its options, silently, is the same
 * defect as a list cut without a marker.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Turns } from "./Turns.tsx";
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

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** Twelve agent seats — four past what the row opens with. */
const roles = Array.from({ length: 12 }, (_, i) => ({
  name: `seat-${i}`,
  kind: "agent" as const,
}));

function mountTurns() {
  const store = new Store();
  store.applyHealth({ status: "healthy" });
  store.applyOrg({ name: "Acme", roles } as never);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () =>
    Promise.resolve({ turns: [], next_cursor: "" });
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Turns />
      </Router>
    </ClientContext.Provider>,
  );
}

test("a roster longer than the row says how many seats it is holding back", async () => {
  mountTurns();
  // The eighth seat is drawn and the ninth is not — that part is the point
  // of opening short.
  await waitFor(() => expect(screen.getByText("seat-7")).toBeTruthy());
  expect(screen.queryByText("seat-8")).toBeNull();
  // And the row SAYS SO, with the count, rather than ending on a chip that
  // reads as the last seat in the company.
  expect(screen.getByText(/4 more seats/i)).toBeTruthy();
});

test("the rest of the roster is one click away, not one URL edit away", async () => {
  mountTurns();
  const more = await waitFor(() => screen.getByText(/4 more seats/i));
  more.click();
  await waitFor(() => expect(screen.getByText("seat-11")).toBeTruthy());
  expect(screen.queryByText(/more seats/i)).toBeNull();
});
