/**
 * What a container's "Filed by" fact says, and what it says when the org chart
 * did not answer.
 *
 * WHO FILES INTO A CONTAINER is read from the org chart — a unit's `space:` is
 * guarded, so the anonymous projection does not carry it — and the fact
 * printed "Needs an operator token to read" for every way that read could fail
 * to answer: to a person signed in without the grant, who lacks a grant rather
 * than a token; to a node still catching up after a restart; and to a read
 * that simply had not come back. Three facts, and only the first is about the
 * reader. It was read from the company document, which holds no units any
 * more, so it answered "no unit" for every container in every company.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ContainerPeek } from "./Knowledge.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { ChartRead } from "~/protocol/index.ts";

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

const container = {
  key: "ENG",
  name: "Engineering",
  pages: 0,
  created_at: "2026-09-01T00:00:00Z",
};

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** The org chart: two units, one filing into the container under another case. */
const chart: ChartRead = {
  units: [
    { key: "engineering", name: "Engineering Unit", space: "eng" },
    { key: "sales", name: "Sales", space: "SALES" },
  ],
  seats: [],
  manages: null,
  leads: null,
  answer: { level: "consistent_prefix", position: "CREWLET_CHART_LOG@1:4" },
  runtime: false,
};

/** The peek, over a socket answering the container and its pages, and `/chart` as told. */
function mount(answer: () => Response | Promise<Response>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) =>
      new URL(String(input)).pathname === "/chart" ? answer() : json({}),
    ),
  );
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = async (what) => {
    if (what === "containers") return { containers: [container] };
    if (what === "pages") return { pages: [], limit: 50 };
    return {};
  };
  return render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <ContainerPeek id="ENG" />
      </Router>
    </ClientContext.Provider>,
  );
}

// A UNIT'S `space:` IS COMPARED CASE-INSENSITIVELY, because the engine
// upper-cases a container key on the way in and whoever wrote the unit wrote
// what they typed. The runtime half is not needed: `space` is on every row.
test("the units whose space names the container are read from the chart", async () => {
  mount(() => json(chart));
  expect(await screen.findByText("Engineering Unit")).toBeDefined();
  // The control: a unit filing somewhere else is not named.
  expect(screen.queryByText(/Sales/)).toBeNull();
});

// THE LINK CARRIES THE UNIT'S KEY. It was built from the unit's NAME, which is
// prose: here the unit is `engineering` and is called "Engineering Unit", so
// the link reached a page that answered "no unit" — and where two units share
// a name it reached whichever the page found first.
test("the one unit filing here links to its page by its key", async () => {
  mount(() => json(chart));
  const name = await screen.findByText("Engineering Unit");
  expect(name.closest("a")?.getAttribute("href")).toBe("#/company/units/engineering");
});

test("a refused chart read names the grant the refusal named", async () => {
  mount(() => json({ error: "unauthorized", reason: "no_grant", grants: ["state:read"] }, 403));
  expect(
    await screen.findByText(
      "Reading who files here needs state:read, which the credential you presented does not carry.",
    ),
  ).toBeDefined();
  expect(screen.queryByText(/operator token/)).toBeNull();
});

test("a node that could not answer is not a refusal of the reader", async () => {
  mount(() => json({ error: "unavailable" }, 503));
  expect(await screen.findByText("The org chart could not be read just now")).toBeDefined();
  expect(screen.queryByText(/needs/)).toBeNull();
});

test("a read still in flight says so", async () => {
  mount(() => new Promise<Response>(() => {}));
  await waitFor(() => expect(screen.getByText("Engineering")).toBeDefined());
  expect(screen.getByText("The org chart has not answered yet")).toBeDefined();
});
