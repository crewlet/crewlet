/**
 * The tool surfaces ask the engine for what the catalogue cannot tell them —
 * once.
 *
 * A tool arrives on a PUSH: the registry is part of the connect snapshot and
 * is re-pushed when a server is re-discovered, so resolving a name costs no
 * request. The single exception is whether the tool's MCP server is shared,
 * which lives in the active configuration — and a header that asked for it and
 * threw the answer away made every addressed tool read the active revision
 * twice, on the page and in the rail alike, server-side parse and redact
 * included, once more on every step through the peek's neighbours.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToolPeek, Tools } from "./Tools.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { ToolAnnotations, ToolRow } from "~/protocol/index.ts";
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

/** Renders a tool surface over a store already holding the pushed catalogue. */
function mount(ui: ReactElement) {
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
  location.hash = "#/";
});

test("a peeked tool reads the server's configuration exactly once", async () => {
  const query = mount(<ToolPeek name="create_issue" />);

  // The body renders what the read answered, so the assertion below is about
  // a request that genuinely happened.
  expect(await screen.findByText(/No seat can call this/)).toBeDefined();
  expect(entityReads(query).length).toBe(1);
});

test("an addressed tool page reads it once too", async () => {
  location.hash = "#/admin/tools/create_issue";
  const query = mount(<Tools tool="create_issue" />);

  expect(await screen.findByText(/No seat can call this/)).toBeDefined();
  expect(entityReads(query).length).toBe(1);
});

// A BUILTIN HAS NO SERVER, so there is nothing to ask and the question is not
// asked — the read is disabled on an empty id rather than sending one the
// engine would refuse.
test("a builtin asks the configuration nothing at all", async () => {
  const query = mount(<ToolPeek name="search_knowledge" />);

  expect(await screen.findByText("Search the company's pages")).toBeDefined();
  expect(entityReads(query).length).toBe(0);
});
