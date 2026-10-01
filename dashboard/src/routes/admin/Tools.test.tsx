/**
 * The tool surfaces ask the engine for what the catalogue cannot tell them —
 * once each — and say which question went unanswered.
 *
 * A tool arrives on a PUSH: the registry is part of the connect snapshot and
 * is re-pushed when a server is re-discovered, so resolving a name costs no
 * request. The two exceptions are whether the tool's MCP server is shared,
 * which lives in the active configuration, and which seats declare
 * credentials for a per-seat server, which is in the org chart's runtime
 * half. A header that asked for the first and threw the answer away made every
 * addressed tool read the active revision twice, on the page and in the rail
 * alike, server-side parse and redact included, once more on every step
 * through the peek's neighbours.
 */

import { cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToolPeek, Tools } from "./Tools.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { ChartRead, ToolAnnotations, ToolRow } from "~/protocol/index.ts";
import type { ReactElement } from "react";

// What the registry advertises for a tool whose server sent no hints: four
// `unknown`s, which is what the engine writes rather than leaving the object
// short — "the server said nothing" is a value here, not a missing field.
const unadvertised: ToolAnnotations = {
  read_only: "unknown",
  destructive: "unknown",
  idempotent: "unknown",
  open_world: "unknown",
};

// Two registrations: one from an MCP server, which is the only origin with a
// configuration entity behind it, and one builtin, which has none at all.
const tools: ToolRow[] = [
  {
    name: "create_issue",
    description: "Open an issue on a repository",
    source: "mcp:github",
    annotations: unadvertised,
    delivers: "github",
  },
  {
    name: "search_knowledge",
    description: "Search the company's pages",
    source: "builtin",
    annotations: unadvertised,
    delivers: "",
  },
];

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** The org chart, served whole to a reader who may read its runtime half. */
const wholeChart: ChartRead = {
  units: [],
  seats: [{ handle: "ceo", name: "CEO", runtime: { llm: "fast" } }],
  manages: null,
  leads: null,
  answer: { level: "consistent_prefix", position: "CREWLET_CHART_LOG@1:4" },
  runtime: true,
};

let chartReads = 0;

/** Installs the engine's REST half as `fetch`, answering `/chart` as told. */
function stubChart(answer: () => Response) {
  chartReads = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input)).pathname;
      if (path !== "/chart") return json({});
      chartReads += 1;
      return answer();
    }),
  );
}

/** Renders a tool surface over a store already holding the pushed catalogue. */
function mount(ui: ReactElement, chart: () => Response = () => json(wholeChart)) {
  stubChart(chart);
  const store = new Store();
  store.applyTools(tools);
  const socket = new LiveSocket(store);
  const query = vi.fn((what: string) =>
    Promise.resolve(
      what === "config_entities"
        ? { kind: "mcp-servers", id: "github", entity: { shared: false } }
        : {},
    ),
  );
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>{ui}</Router>
    </ClientContext.Provider>,
  );
  return query;
}

/** Every `config_entities` frame this mount sent. */
function entityReads(query: ReturnType<typeof mount>) {
  return query.mock.calls.filter(([what]) => what === "config_entities");
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

test("a peeked tool reads the server's configuration exactly once", async () => {
  const query = mount(<ToolPeek name="create_issue" />);

  // The body renders what the read answered, so the assertion below is about
  // a request that genuinely happened.
  expect(await screen.findByText(/No seat can call this/)).toBeDefined();
  expect(entityReads(query).length).toBe(1);
  expect(chartReads).toBe(1);
});

test("an addressed tool page reads it once too", async () => {
  location.hash = "#/admin/tools/create_issue";
  const query = mount(<Tools tool="create_issue" />);

  expect(await screen.findByText(/No seat can call this/)).toBeDefined();
  expect(entityReads(query).length).toBe(1);
  expect(chartReads).toBe(1);
});

// WHICH READ WENT UNANSWERED IS NAMED. Whether a server is shared is the
// configuration's answer, and which seats declare its credentials is the org
// chart's; the panel blamed "the active configuration" for both, while the
// configuration had answered and the chart had withheld its runtime half.
test("a runtime half the chart withheld is named, and the configuration is not blamed", async () => {
  mount(<ToolPeek name="create_issue" />, () => json({ ...wholeChart, seats: [], runtime: false }));

  expect(
    await screen.findByText(/in the org chart's runtime half, which was not shown to you/),
  ).toBeDefined();
  expect(screen.queryByText(/active configuration did not answer/)).toBeNull();
  expect(screen.queryByText(/No seat can call this/)).toBeNull();
});

test("a refused chart read names the grant it named", async () => {
  mount(<ToolPeek name="create_issue" />, () =>
    json({ error: "unauthorized", reason: "no_grant", grants: ["state:read"] }, 403),
  );

  expect(
    await screen.findByText(
      "Reading which seats declare credentials for it needs state:read, which the credential you presented does not carry.",
    ),
  ).toBeDefined();
  expect(screen.queryByText(/No seat can call this/)).toBeNull();
});

// A BUILTIN HAS NO SERVER, so there is nothing to ask and the question is not
// asked — the read is disabled on an empty id rather than sending one the
// engine would refuse.
test("a builtin asks the configuration nothing at all", async () => {
  const query = mount(<ToolPeek name="search_knowledge" />);

  expect(await screen.findByText("Search the company's pages")).toBeDefined();
  expect(entityReads(query).length).toBe(0);
  expect(chartReads).toBe(0);
});
