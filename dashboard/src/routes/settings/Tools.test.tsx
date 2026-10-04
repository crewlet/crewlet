/**
 * Settings › Tools & MCP: the catalogue, the servers and what every node did
 * with them, and the form that adds one.
 *
 * The invariants, in the order they cost when they go:
 *
 *  - A TOOL ASKS THE ENGINE NOTHING. The registry arrives on a push and who
 *    holds a tool is each agent seat's `tool_sources` on the pushed org — the
 *    engine's own grant — so a peek, a page and every step through the
 *    neighbours cost no request. They used to read the active configuration
 *    twice per tool and re-derive the `mcp_env` rule from it.
 *  - Each server's state and its per-node cells are the ENGINE'S; a node
 *    whose build does not report is unknown, never "none".
 *  - The servers are operator-only and the catalogue is not: a refused
 *    reader sees the refusal in the servers' place and every tool around it.
 *  - An add is the create-only PUT, checked first with the company's own
 *    warnings subtracted, and a refusal is placed beside its field.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { ReactElement } from "react";

import { ToolPeek, Tools } from "./Tools.tsx";
import { nodeCellWords } from "./McpServers.tsx";
import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, QueryError, Store } from "~/protocol/index.ts";
import type { ToolAnnotations, ToolRow } from "~/protocol/index.ts";
import type { McpServersStatusAnswer } from "~/contract/mcp.ts";

// What the registry advertises for a tool whose server sent no hints: four
// `unknown`s, which is what the engine writes rather than leaving the object
// short — "the server said nothing" is a value here, not a missing field.
const unadvertised: ToolAnnotations = {
  read_only: "unknown",
  destructive: "unknown",
  idempotent: "unknown",
  open_world: "unknown",
};

const tools: ToolRow[] = [
  {
    name: "create_issue",
    description: "Open an issue on a repository",
    source: "mcp:github",
    annotations: unadvertised,
    delivers: "github",
  },
  {
    name: "search_docs",
    description: "Search the docs site",
    source: "mcp:docs",
    annotations: unadvertised,
    delivers: "",
  },
  {
    name: "search_knowledge",
    description: "Search the company's pages",
    source: "builtin",
    annotations: unadvertised,
    delivers: "",
  },
];

/** Two agents: github granted to one, docs to both. */
const org = {
  name: "Acme",
  roles: [
    {
      name: "Backend Dev",
      handle: "backend-dev",
      tool_sources: ["builtin", "mcp:docs", "mcp:github"],
    },
    { name: "PM", handle: "pm", tool_sources: ["builtin", "mcp:docs"] },
  ],
};

const status: McpServersStatusAnswer = {
  nodes: [
    { id: "node-a", reported: true },
    { id: "node-b", reported: true },
    { id: "node-c", reported: false },
  ],
  servers: [
    {
      name: "docs",
      configured: true,
      shared: true,
      transport: "stdio",
      command: "docs-mcp",
      args: ["--read-only"],
      url: "",
      state: "running",
      started: 2,
      failed: 0,
      tools: 4,
      nodes: [
        {
          node: "node-a",
          reported: true,
          started: 1,
          failed: 0,
          tools: 4,
          error: "",
          error_seat: "",
        },
        {
          node: "node-b",
          reported: true,
          started: 1,
          failed: 0,
          tools: 4,
          error: "",
          error_seat: "",
        },
        {
          node: "node-c",
          reported: false,
          started: 0,
          failed: 0,
          tools: 0,
          error: "",
          error_seat: "",
        },
      ],
    },
    {
      name: "github",
      configured: true,
      shared: false,
      transport: "stdio",
      command: "npx",
      args: ["-y", "@modelcontextprotocol/server-github"],
      url: "",
      state: "partial",
      started: 1,
      failed: 1,
      tools: 26,
      nodes: [
        {
          node: "node-a",
          reported: true,
          started: 1,
          failed: 0,
          tools: 26,
          error: "",
          error_seat: "",
        },
        {
          node: "node-b",
          reported: true,
          started: 0,
          failed: 1,
          tools: 0,
          error: "401 Bad credentials",
          error_seat: "backend-dev",
        },
        {
          node: "node-c",
          reported: false,
          started: 0,
          failed: 0,
          tools: 0,
          error: "",
          error_seat: "",
        },
      ],
    },
    {
      // A SERVER THAT REGISTERED NOTHING: it failed on the one node that
      // launched it, so the catalogue has no row from it at all.
      name: "notes",
      configured: true,
      shared: true,
      transport: "stdio",
      command: "notes-mcp",
      args: [],
      url: "",
      state: "failing",
      started: 0,
      failed: 1,
      tools: 0,
      nodes: [
        {
          node: "node-a",
          reported: true,
          started: 0,
          failed: 1,
          tools: 0,
          error:
            'mcp: server "notes" failed to connect: exec: "notes-mcp": executable file not found in $PATH',
          error_seat: "",
        },
      ],
    },
  ],
};

const OPERATOR = {
  login: "U0FOUNDER",
  grants: [
    "config:read",
    "config:write",
    "secrets:write",
    "fleet:operate",
    "people:manage",
    "audit:read",
    "state:read",
    "work:write",
    "knowledge:write",
  ],
  handle: "jane",
  owner: "jane",
  name: "Jane",
  kind: "human",
};
const READER = { login: "", grants: [], handle: "", owner: "", name: "", kind: "" };

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/**
 * Renders a tool surface over a store holding the pushed catalogue and org,
 * and answers the socket's questions: the viewer, and the server status (or
 * the refusal an anonymous reader gets).
 */
function mount(
  ui: ReactElement,
  {
    viewer = OPERATOR,
    refuse = false,
    roster = org,
    answer = status,
  }: {
    viewer?: Record<string, unknown>;
    refuse?: boolean;
    roster?: unknown;
    answer?: McpServersStatusAnswer;
  } = {},
) {
  const store = new Store();
  store.applyTools(tools);
  store.applyOrg(roster as never);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  const query = vi.fn((what: string) => {
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "mcp_servers_status") {
      return refuse ? Promise.reject(new QueryError("unauthorized")) : Promise.resolve(answer);
    }
    return new Promise(() => {});
  });
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = query;
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ToastProvider>
        <LayerHost>
          <FrameReadings>
            <Router>{ui}</Router>
          </FrameReadings>
        </LayerHost>
      </ToastProvider>
    </ClientContext.Provider>,
  );
  return query;
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  });
}

/**
 * Every question this mount asked about tools, servers or the configuration —
 * the frame's own readings (the viewer, the inbox count) are not the tool's.
 */
function toolReads(query: ReturnType<typeof mount>) {
  return query.mock.calls
    .map(([what]) => what)
    .filter((what) => what.startsWith("config") || what === "mcp_servers_status");
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

// A TOOL ASKS NOTHING: its holders are the pushed org's grant.
test("a peeked tool names its holders from the pushed grant and asks the engine nothing", async () => {
  const query = mount(<ToolPeek name="create_issue" />);
  await settle();
  expect(screen.getByText("Backend Dev")).toBeDefined();
  expect(screen.queryByText("PM")).toBeNull();
  expect(toolReads(query)).toEqual([]);
});

test("a server granted to every agent seat says so", async () => {
  mount(<ToolPeek name="search_docs" />);
  await settle();
  expect(screen.getByText(/Every agent seat — 2 seats/)).toBeDefined();
});

// A TEMPLATE NOBODY IS GRANTED is a finding, said outright.
test("a server granted to nobody says no seat can call it", async () => {
  mount(<ToolPeek name="create_issue" />, {
    roster: { name: "Acme", roles: [{ name: "PM", handle: "pm", tool_sources: ["builtin"] }] },
  });
  await settle();
  expect(screen.getByText(/No seat can call this/)).toBeDefined();
});

// UNKNOWN IS NOT NOBODY: a roster with no grant on it came from an older node.
test("a roster that carries no grant is unknown, not nobody", async () => {
  mount(<ToolPeek name="create_issue" />, {
    roster: { name: "Acme", roles: [{ name: "PM", handle: "pm" }] },
  });
  await settle();
  expect(screen.queryByText(/No seat can call this/)).toBeNull();
  expect(screen.getByText(/is not on the roster/)).toBeDefined();
});

// THE LIST IS FOR FINDING A TOOL. A description is written for a model and
// runs to a paragraph, and unclamped every row of the catalogue was a column of
// prose. It is clamped, and the whole sentence stays reachable.
test("the catalogue clamps a description and keeps the whole of it in reach", async () => {
  mount(<Tools />);
  await settle();
  const cell = await screen.findByText("Open an issue on a repository");
  expect(cell.classList.contains("clamp")).toBe(true);
  expect(cell.getAttribute("title")).toBe("Open an issue on a repository");
});

// PER-SERVER, PER-NODE, AS THE ENGINE SENT IT — the state its word, each node
// its cell, the failing node's reason and seat under it, and the node that
// does not report named once and drawn unknown rather than as "none".
test("each server is drawn with the engine's state and a cell per node", async () => {
  mount(<Tools />);
  await settle();
  const github = screen.getByText("Partly failing").closest(".grid-row") as HTMLElement;
  expect(within(github).getByText("Partly failing")).toBeDefined();
  expect(within(github).getByText(/401 Bad credentials/)).toBeDefined();
  expect(within(github).getByText(/backend-dev/)).toBeDefined();
  expect(within(github).getAllByText(/not reported/).length).toBe(1);
  expect(within(github).getByText("1 seat")).toBeDefined();
  const docs = screen.getByText("Running").closest(".grid-row") as HTMLElement;
  expect(within(docs).getByText("Running")).toBeDefined();
  expect(within(docs).getByText("Every agent seat")).toBeDefined();
  expect(screen.getByText(/runs a build that does not report its MCP servers/)).toBeDefined();
});

test("a node's cell never reads an unreported node as none", () => {
  const cell = {
    node: "n",
    reported: false,
    started: 0,
    failed: 0,
    tools: 0,
    error: "",
    error_seat: "",
  };
  expect(nodeCellWords(cell).text).toBe("not reported");
  expect(nodeCellWords({ ...cell, reported: true }).text).toBe("none here");
  expect(nodeCellWords({ ...cell, reported: true, started: 1, failed: 1 }).tone).toBe("warning");
  expect(nodeCellWords({ ...cell, reported: true, failed: 2 }).tone).toBe("danger");
});

// THE REASON SITS UNDER ITS NODE'S CHIP and does not restate the node: the
// chip names it, and the reason is prose in the secondary ink — the chip alone
// carries the danger tone.
test("a node's failure sits under its chip without repeating the node", async () => {
  mount(<Tools />);
  await settle();
  const github = screen.getByText("Partly failing").closest(".grid-row") as HTMLElement;
  const reason = within(github).getByText("for backend-dev: 401 Bad credentials");
  expect(reason.classList.contains("mcp-failure")).toBe(true);
  expect(reason.textContent?.startsWith("node-b")).toBe(false);
});

// UNKNOWN IS NOT NOBODY, in the servers' grid too: a per-seat server on a
// roster with no grant on it is unknown, while a shared one reaches every
// agent seat by the engine's own rule whatever the roster carries.
test("a per-seat server's reach on a roster without the grant is unknown", async () => {
  mount(<Tools />, { roster: { name: "Acme", roles: [{ name: "PM", handle: "pm" }] } });
  await settle();
  const github = screen.getByText("Partly failing").closest(".grid-row") as HTMLElement;
  const reach = within(github).getByText("Unknown: this node's roster does not say");
  expect(reach.classList.contains("muted")).toBe(true);
  expect(within(github).queryByText(/Not on this roster|No seat/)).toBeNull();
  const docs = screen.getByText("Running").closest(".grid-row") as HTMLElement;
  expect(within(docs).getByText("Every agent seat")).toBeDefined();
});

// ONE ANSWER TO "HOW MANY SERVERS": the tile counts the engine's servers, as
// the table under it does, never the registry's origins — which read "0" over
// a table of failing servers.
test("the MCP tile counts the servers the engine reports, as the table does", async () => {
  mount(<Tools />);
  await settle();
  expect(screen.getByText("2 of 3 servers running")).toBeDefined();
  expect(screen.queryByText(/server\(s\)/)).toBeNull();
});

// A SERVER'S PAGE IS NOT AN EMPTY SEARCH. A row opens `servers/{name}`; for a
// server with no tools that filtered by an origin no chip named, and said
// "No tool matches “”" with nothing quoted. It opens the server — its state
// and every failure whole — with its chip selected, an empty state that says
// why, and a way back to every tool.
test("a server with no tools opens its page, never an empty quoted search", async () => {
  location.hash = "#/settings/tools/servers/notes";
  mount(<Tools server="notes" />);
  await settle();
  expect(screen.queryByText(/“”/)).toBeNull();
  expect(screen.getByRole("heading", { name: "notes" })).toBeDefined();
  // The whole reason, not a clamp of it.
  expect(
    screen.getByText(/executable file not found in \$PATH/, { selector: ".mcp-failure-whole" }),
  ).toBeDefined();
  // The filter in force has a chip, and it is the chosen one.
  const chip = screen.getByRole("radio", { name: /mcp:notes/ });
  expect(chip.getAttribute("aria-checked")).toBe("true");
  expect(screen.getByText("notes has registered no tools")).toBeDefined();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Show all tools" }));
  });
  await settle();
  expect(location.hash.startsWith("#/settings/tools")).toBe(true);
  expect(location.hash).not.toContain("servers");
});

// A SEARCH THAT FOUND NOTHING still quotes what was searched.
test("a search that matches nothing quotes the search", async () => {
  location.hash = "#/settings/tools?q=zzz";
  mount(<Tools />);
  await settle();
  expect(screen.getByText("No tool matches “zzz”")).toBeDefined();
});

// THE SERVERS ARE THE OPERATOR'S AND THE CATALOGUE IS EVERYBODY'S.
test("a refused reader sees the refusal where the servers were, and every tool", async () => {
  mount(<Tools />, { viewer: READER, refuse: true });
  await settle();
  expect(screen.queryByText("Partly failing")).toBeNull();
  expect(screen.getByText("Open an issue on a repository")).toBeDefined();
  // The add is drawn, and disabled with the reason.
  const add = screen.getByRole("button", { name: /Add an MCP server/ });
  expect(add.getAttribute("aria-disabled") === "true" || add.hasAttribute("disabled")).toBe(true);
});

// NOT BEING TOLD IS NOT BEING TOLD "NONE". A reader refused the operator-only
// status who opens a server's page was told the server does not exist, right
// above the section that says the answer was refused.
test("a refused reader on a server's page is never told the server does not exist", async () => {
  location.hash = "#/settings/tools/servers/github";
  mount(<Tools server="github" />, { viewer: READER, refuse: true });
  await settle();
  expect(screen.getByRole("heading", { name: "github" })).toBeDefined();
  expect(screen.queryByText(/No configuration this engine can see carries/)).toBeNull();
  expect(screen.getByText(/status could not be read here/)).toBeDefined();
});

// …and a reader who WAS told, about a name nothing carries, is told so.
test("a server's page for a name the answer does not carry says so", async () => {
  location.hash = "#/settings/tools/servers/nowhere";
  mount(<Tools server="nowhere" />);
  await settle();
  expect(screen.getByText(/No configuration this engine can see carries/)).toBeDefined();
  expect(screen.queryByText(/status could not be read here/)).toBeNull();
});

// --- adding one ---------------------------------------------------------- //

const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

interface Call {
  method: string;
  url: string;
  body: Record<string, unknown> | undefined;
  headers: Headers;
}

/**
 * Stub the configuration surface; `respond` answers each request to it. The
 * secret store's name list, which the form reads for its `$` completion, is
 * answered empty and not recorded: it is not a write.
 */
function stubConfig(respond: (call: Call) => Response): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      if (String(url).includes("/secrets")) return json({ secrets: [] }, 200);
      const call: Call = {
        method: init.method ?? "GET",
        url: String(url),
        body: init.body ? (JSON.parse(init.body as string) as Record<string, unknown>) : undefined,
        headers: new Headers(init.headers),
      };
      calls.push(call);
      return respond(call);
    }),
  );
  return calls;
}

const company = { name: "Acme", mcp_servers: [{ name: "docs", command: "docs-mcp" }] };
const valid = (warnings: unknown[] = []) =>
  json({ valid: true, base_revision_id: "r1", warnings, derived: null }, 200);

async function openAdd() {
  location.hash = "#/settings/tools?add=server";
  mount(<Tools />);
  await settle();
  return screen.getByRole("dialog");
}

function type(dialog: HTMLElement, label: string, value: string) {
  fireEvent.change(within(dialog).getByLabelText(new RegExp(`^${label}`)), { target: { value } });
}

async function press(dialog: HTMLElement, name: RegExp) {
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("button", { name }));
  });
  await settle();
}

test("an add is checked against the company, then stored create-only", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(company, 200, { ETag: '"r1"' });
    if (call.url.includes("dry_run=true")) return valid();
    return json({ revision_id: "r2", epoch: 5, warnings: [], derived: null }, 201);
  });
  const dialog = await openAdd();
  type(dialog, "Name", "linear");
  type(dialog, "Command", "linear-mcp");
  type(dialog, "Arguments", "--stdio\n\n--team eng");
  await press(dialog, /Add server/);

  const writes = calls.map((c) => `${c.method} ${c.url.replace(/^.*\/config/, "/config")}`);
  expect(writes).toEqual([
    "GET /config",
    "PATCH /config?dry_run=true",
    "PUT /config/mcp-servers/linear?dry_run=true",
    "PUT /config/mcp-servers/linear",
  ]);
  const save = calls[3]!;
  expect(save.headers.get("If-None-Match")).toBe("*");
  expect(save.headers.get("If-Match")).toBeNull();
  expect(save.body).toEqual({
    name: "linear",
    transport: "stdio",
    command: "linear-mcp",
    args: ["--stdio", "--team eng"],
    _summary: "Add the MCP server linear (stdio, shared)",
  });
  expect(screen.queryByRole("dialog")).toBeNull();
});

// ONLY WHAT THE ADD INTRODUCES STOPS IT: the company's own warning is not
// this server's, and a warning that IS stops the save until said again.
test("a warning the add introduces is said before it is stored", async () => {
  const old = { kind: "dangling_reference", path: "roles[3].manages[0]", message: "old" };
  const fresh = { kind: "admission", path: "mcp_servers[1]", message: "nobody is granted linear" };
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(company, 200, { ETag: '"r1"' });
    if (call.method === "PATCH") return valid([old]);
    if (call.url.includes("dry_run=true")) return valid([old, fresh]);
    return json({ revision_id: "r2", epoch: 5, warnings: [], derived: null }, 201);
  });
  const dialog = await openAdd();
  type(dialog, "Name", "linear");
  type(dialog, "Command", "linear-mcp");
  await press(dialog, /Add server/);
  expect(within(dialog).getByText("nobody is granted linear")).toBeDefined();
  expect(within(dialog).queryByText("old")).toBeNull();
  expect(calls.filter((c) => !c.url.includes("dry_run") && c.method === "PUT")).toEqual([]);
  await press(dialog, /Add anyway/);
  expect(calls.filter((c) => !c.url.includes("dry_run") && c.method === "PUT").length).toBe(1);
});

// THE ENGINE'S PROBLEM GOES BESIDE ITS FIELD, by the problem's segments.
test("a refused add places the engine's problem beside the field it is about", async () => {
  stubConfig((call) => {
    if (call.method === "GET") return json(company, 200, { ETag: '"r1"' });
    if (call.method === "PATCH") return valid();
    return json(
      {
        error: "validation_error",
        problems: [
          {
            path: "mcp_servers[1].url",
            segments: ["mcp_servers", 1, "url"],
            kind: "missing",
            message: "an http server needs a URL the engine can reach",
          },
        ],
      },
      400,
    );
  });
  const dialog = await openAdd();
  type(dialog, "Name", "linear");
  await act(async () => {
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Transport" }));
  });
  fireEvent.mouseDown(screen.getByRole("option", { name: /Connect to an address/ }));
  type(dialog, "Address", "mcp.example.com");
  await press(dialog, /Add server/);
  expect(within(dialog).getByText("an http server needs a URL the engine can reach")).toBeDefined();
});

// A NAME THE SCREEN ALREADY SHOWS is refused before any request — and a name
// somebody took since is the engine's `entity_exists`, said as such.
test("a taken name is refused before the round trip, and one taken since is said", async () => {
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(company, 200, { ETag: '"r1"' });
    if (call.method === "PATCH") return valid();
    return json({ error: "entity_exists" }, 412);
  });
  const dialog = await openAdd();
  type(dialog, "Name", "github");
  type(dialog, "Command", "x");
  await press(dialog, /Add server/);
  expect(within(dialog).getByText(/A server called github already exists/)).toBeDefined();
  expect(calls).toEqual([]);

  type(dialog, "Name", "linear");
  await press(dialog, /Add server/);
  expect(within(dialog).getByText(/was added since this form opened/)).toBeDefined();
});

// A NAME ONLY A NODE STILL RUNS is free: the engine's create is judged against
// the configuration, so refusing it here refused what the engine allows, and
// said "already exists" of a server the company does not carry.
test("a name only a node still reports is said, not refused", async () => {
  const answer: McpServersStatusAnswer = {
    ...status,
    servers: [
      ...status.servers,
      { ...status.servers[0]!, name: "legacy", configured: false, command: "", args: [] },
    ],
  };
  const calls = stubConfig((call) => {
    if (call.method === "GET") return json(company, 200, { ETag: '"r1"' });
    if (call.method === "PATCH") return valid();
    return json({}, 200, { ETag: '"r2"' });
  });
  location.hash = "#/settings/tools?add=server";
  mount(<Tools />, { answer });
  await settle();
  const dialog = screen.getByRole("dialog");
  type(dialog, "Name", "legacy");
  type(dialog, "Command", "x");
  expect(within(dialog).getByText(/still runs a server called/)).toBeDefined();
  await press(dialog, /Add server/);
  expect(within(dialog).queryByText(/already exists/)).toBeNull();
  expect(calls.length).toBeGreaterThan(0);
});

// A ROW IS ITS OWN IDENTITY, NOT ITS POSITION. Keyed by index, removing the
// first of two rows handed the second row's values to the first row's field —
// and with it that field's own state (an open `$` completion, its caret).
test("removing an environment row keeps every other row's field its own", async () => {
  stubConfig(() => json(company, 200, { ETag: '"r1"' }));
  const dialog = await openAdd();
  await press(dialog, /Add variable/);
  await press(dialog, /Add variable/);
  const second = within(dialog).getByLabelText(/^Variable 2 name/);
  fireEvent.change(second, { target: { value: "SECOND" } });
  await press(dialog, /Remove variable 1/);
  const left = within(dialog).getByLabelText(/^Variable 1 name/) as HTMLInputElement;
  expect(left.value).toBe("SECOND");
  expect(left).toBe(second);
});
