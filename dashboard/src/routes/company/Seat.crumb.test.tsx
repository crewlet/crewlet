/**
 * What the page bar calls a seat, and what the browser tab calls it.
 *
 * A HANDLE IS DERIVED FROM A NAME — `agent-cto` for "Chief Technology Officer" —
 * so the trail read "Company / People / agent-cto" over a header titled with the
 * name, and `Shell` titles the browser tab from that same trail, so a reader
 * with four tabs open had four slugs. Everywhere else in the product a seat is
 * drawn by name with the handle beside it as the identifier; the crumb was the
 * single place that named a seat something nobody calls it.
 *
 * The machinery was right and the seat screen was the one object screen that
 * never held up its end of it: `crumbsFor` takes labels a screen publishes, and
 * `Seat.tsx` published none.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SeatScreen } from "./Seat.tsx";
import { Shell } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { OrgProjection } from "~/protocol/index.ts";

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
  document.title = "";
});

async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
    await Promise.resolve();
  });
}

function mount(hash: string, org: OrgProjection, handle: string) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: () => Promise<unknown> }).query = () => Promise.resolve({});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Shell>
          <SeatScreen handle={handle} />
        </Shell>
      </Router>
    </ClientContext.Provider>,
  );
}

const trail = () => screen.getByRole("navigation", { name: "Breadcrumb" });

// THE TWO STRINGS HAVE TO DIFFER or the assertion cannot fail.
test("the seat's breadcrumb and browser tab name the seat, not the handle the URL carries", async () => {
  mount(
    "#/company/people/agent-cto",
    { name: "Acme", roles: [{ name: "Chief Technology Officer", handle: "agent-cto" }] },
    "agent-cto",
  );
  await settle();

  const here = trail().querySelector("[aria-current='page']");
  expect(here?.textContent).toBe("Chief Technology Officer");
  expect(trail().textContent, "the trail still names the slug").not.toContain("agent-cto");
  // AND THE TAB IS TITLED FROM THE SAME TRAIL.
  expect(document.title).toBe("Chief Technology Officer · Crewlet");
});

// A SEAT THE ENGINE REPORTED NO HANDLE FOR IS ADDRESSED BY NAME — `seatPath`
// says so — so the label has to be keyed on the RAW SEGMENT rather than on
// `seat.handle`, which is "" there and would publish a blank crumb.
test("a seat addressed by name still ends its trail in the name", async () => {
  mount(
    "#/company/people/Chief%20Technology%20Officer",
    { name: "Acme", roles: [{ name: "Chief Technology Officer" }] },
    "Chief Technology Officer",
  );
  await settle();

  const here = trail().querySelector("[aria-current='page']");
  expect(here?.textContent).toBe("Chief Technology Officer");
});
