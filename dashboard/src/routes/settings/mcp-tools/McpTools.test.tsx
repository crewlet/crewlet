/**
 * Settings › MCP & tools as each reader meets it: the list's header, tabs,
 * figures, alarms and table, a server's page, and every flow by its address,
 * in an administrator's reading and a member's.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import type { RestResult } from "~/lib/useRest.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import type { ServerDetail } from "./api.ts";
import type { McpData } from "./data.ts";
import {
  ADMIN_STATUS,
  LINEAR_APPROVAL,
  LINEAR_DETAIL,
  LINEAR_MEMBER_DETAIL,
  LINEAR_OAUTH,
  MCP_NOW,
  mcpData,
  withOAuth,
} from "./fixtures.ts";
import { McpTools } from "./McpTools.tsx";

let current: McpData;
let detail: ServerDetail | null;
vi.mock("./data.ts", async (actual) => ({
  ...(await actual<typeof import("./data.ts")>()),
  useMcpData: () => current,
  useServerDetail: (): RestResult<ServerDetail> => ({
    data: detail,
    error: null,
    code: null,
    refusal: null,
    loading: false,
    reload: async () => {},
  }),
}));
// NOTHING IS SENT: every write is a request that never answers, so a flow
// that opens stays open for the test to see, and none completes.
vi.mock("./api.ts", async (actual) => {
  const real = await actual<typeof import("./api.ts")>();
  const pending = () => new Promise(() => {});
  return {
    ...real,
    restartServer: vi.fn(pending),
    setEnabled: vi.fn(pending),
    removeServer: vi.fn(pending),
    testServer: vi.fn(pending),
    testDraft: vi.fn(pending),
    probeAddress: vi.fn(pending),
    storeValue: vi.fn(pending),
    readServer: vi.fn(pending),
    readCatalogue: vi.fn(async () => ({
      reading: "admin",
      curated: [],
      community: [],
      registry: "",
      reason: "",
      left_out: 0,
    })),
  };
});
vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  getEntity: vi.fn(async () => ({
    kind: "entity",
    entity: { name: "linear", transport: "http", url: "https://mcp.linear.app/mcp" },
    etag: '"entry-r1"',
  })),
}));

/** Every address a flow opens at, on the list and on a server's page. */
const ADDRESSES: { hash: string; server?: string }[] = [
  { hash: "#/settings/mcp-tools" },
  { hash: "#/settings/mcp-tools?view=agents" },
  { hash: "#/settings/mcp-tools?view=builtin" },
  { hash: "#/settings/mcp-tools?flow=approve&server=linear" },
  { hash: "#/settings/mcp-tools?flow=add" },
  { hash: "#/settings/mcp-tools?flow=edit&server=linear&step=connection" },
  { hash: "#/settings/mcp-tools?flow=test&server=linear" },
  { hash: "#/settings/mcp-tools?flow=value&server=linear&key=Authorization" },
  { hash: "#/settings/mcp-tools?flow=restart&server=linear" },
  { hash: "#/settings/mcp-tools?flow=disable&server=linear" },
  { hash: "#/settings/mcp-tools?flow=remove&server=linear" },
  { hash: "#/settings/mcp-tools?flow=signin&server=linear" },
  { hash: "#/settings/mcp-tools?flow=revoke&server=linear" },
  { hash: "#/settings/mcp-tools/servers/linear", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?tab=connection", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?tab=tools", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?tab=access", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?tab=activity", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?flow=approve", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?flow=restart", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?flow=remove", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?flow=test", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?flow=edit&step=signin", server: "linear" },
];

/** What only an administrator may meet: a flow's surface, or a control that opens one. */
function writeControls(): string[] {
  const found: string[] = [];
  for (const dialog of screen.queryAllByRole("dialog")) {
    found.push(`dialog "${dialog.getAttribute("aria-label") ?? dialog.textContent?.slice(0, 30)}"`);
  }
  for (const name of [
    /^Add server$/,
    /^Restart$/,
    /^Actions for /,
    /^Edit/,
    /^Test now$/,
    /^Allow once$/,
    /^Save access$/,
  ]) {
    if (screen.queryAllByRole("button", { name }).length > 0) found.push(`button ${name}`);
  }
  // THE WIZARD is a page of its own, in the list's place.
  for (const name of [/^Add an MCP server$/, /^Edit /]) {
    if (screen.queryAllByRole("heading", { name }).length > 0) found.push(`wizard ${name}`);
  }
  return found;
}

function mountAt(address: { hash: string; server?: string }) {
  location.hash = address.hash;
  render(
    <TestClient>
      <ViewerProvider>
        <LayerHost>
          <Router>
            <McpTools {...(address.server ? { server: address.server } : {})} />
          </Router>
        </LayerHost>
      </ViewerProvider>
    </TestClient>,
  );
}

beforeEach(() => {
  vi.useFakeTimers({ now: MCP_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

test("a member meets no write control at any address, a flow's included", () => {
  current = mcpData("member");
  detail = LINEAR_MEMBER_DETAIL;
  for (const address of ADDRESSES) {
    mountAt(address);
    expect(writeControls(), address.hash).toEqual([]);
    cleanup();
  }
});

// THE OTHER SIDE, so the reading above cannot pass by finding nothing at all:
// the same addresses draw an administrator's controls.
test("an administrator meets the controls and the flows at the same addresses", async () => {
  // Linear signed in to with OAuth, so its sign-in and its revocation open.
  current = withOAuth(mcpData("admin"), "linear", LINEAR_OAUTH);
  detail = LINEAR_DETAIL;
  for (const address of ADDRESSES) {
    mountAt(address);
    // An edit opens once the entry it edits is read: every stub has answered,
    // and what it drew, at the task boundary.
    await afterMicrotasks();
    expect(writeControls().length, address.hash).toBeGreaterThan(0);
    cleanup();
  }
});

// THE LIST: the header names the page and its one action, the tabs its
// views (Agent access the administrator's), and a member reads that the page
// is theirs to read only.
test("the list names the page, its views and, for a member, why it is read only", () => {
  current = mcpData("admin");
  mountAt({ hash: "#/settings/mcp-tools" });
  const tabs = screen.getByRole("tablist", { name: "MCP & tools sections" });
  expect(within(tabs).getByRole("tab", { name: /Servers/ })).toBeDefined();
  expect(within(tabs).getByRole("tab", { name: /Agent access/ })).toBeDefined();
  expect(within(tabs).getByRole("tab", { name: /Built-in tools/ })).toBeDefined();
  expect(screen.queryByText("Read-only. Changes take Admin access.")).toBeNull();
  // FOUR FIGURES for an administrator, the fourth the calls waiting on them.
  expect(document.querySelectorAll(".crewlet-statcard")).toHaveLength(4);
  expect(screen.getByText("Waiting on you")).toBeDefined();
  expect(screen.getByText("no call or change to review")).toBeDefined();
  cleanup();

  current = mcpData("member");
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(screen.getByText("Read-only. Changes take Admin access.")).toBeDefined();
  expect(document.querySelectorAll(".crewlet-statcard")).toHaveLength(3);
  expect(
    within(screen.getByRole("tablist", { name: "MCP & tools sections" })).queryByRole("tab", {
      name: /Agent access/,
    }),
  ).toBeNull();
  cleanup();

  // AGENT ACCESS IS THE ADMINISTRATOR'S: its address opens the servers for a member.
  mountAt({ hash: "#/settings/mcp-tools?view=agents" });
  expect(screen.getByText("Read-only. Changes take Admin access.")).toBeDefined();
});

// THE ENGINE'S ALARMS, a row each with the fix the engine names: a restart is
// carried out here, an edit opens the wizard where the prefix is.
test("each alarm's row carries the fix the engine names for it", () => {
  current = mcpData("admin");
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(screen.getByText("Linear is not running on node-b")).toBeDefined();
  expect(screen.getByText("1 search tool is not offered")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Restart" }));
  expect(location.hash).toContain("flow=restart");
  expect(location.hash).toContain("server=linear");
  const edit = screen.getByRole("link", { name: "Change its prefix" });
  expect(edit.getAttribute("href")).toBe(
    "#/settings/mcp-tools/servers/search?flow=edit&step=connection",
  );
});

// A SIGN-IN ALARM'S FIX IS THE SIGN-IN, opened in place rather than a link to
// the tab it is made from. Mutation: link the Connection tab again, and this
// is red.
test("an alarm a sign-in fixes opens the Sign in dialog", () => {
  current = {
    ...mcpData("admin"),
    alarms: [
      {
        kind: "mcp_oauth_needs_sign_in",
        detail: "linear needs a sign-in",
        remedy: "",
        entries: [
          {
            key: "linear",
            detail: "nobody has signed in",
            since: null,
            seat: "",
            fix: { kind: "sign_in", server: "linear", seat: "" },
          },
        ],
      },
    ],
  };
  mountAt({ hash: "#/settings/mcp-tools" });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  expect(location.hash).toContain("flow=signin");
  expect(location.hash).toContain("server=linear");
});

// THE TABLE draws the engine's words: the state, where a remote server is,
// which agents the engine grants it, and the class of what went wrong.
test("a server's row says its condition, its reach and what went wrong in the engine's terms", () => {
  current = mcpData("admin");
  mountAt({ hash: "#/settings/mcp-tools" });
  const rowOf = (text: string) => {
    const row = screen.getAllByText(text)[0]?.closest("tr, [role='row']");
    if (!(row instanceof HTMLElement)) throw new Error(`no row holds ${text}`);
    return row;
  };
  const linear = rowOf("Linear");
  expect(within(linear).getByText("Partial")).toBeDefined();
  expect(within(linear).getByText("1 of 2")).toBeDefined();
  expect(within(linear).getByText("mcp.linear.app")).toBeDefined();
  expect(within(linear).getByText("Credential rejected")).toBeDefined();
  expect(within(linear).getByText("Every agent")).toBeDefined();
  const search = rowOf("search");
  expect(within(search).getByText("Backend Engineer")).toBeDefined();
  expect(within(search).getByText("1 not offered")).toBeDefined();
  const docs = rowOf("docs");
  expect(within(docs).getByText("Disabled")).toBeDefined();
  expect(within(docs).getByText("docs-mcp --read-only")).toBeDefined();
  cleanup();

  // A MEMBER reads what a server is for, never its address or command, and
  // no reach, which the engine does not push them: never "No agent".
  current = mcpData("member");
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(screen.queryByText("Used by")).toBeNull();
  expect(screen.queryByText("No agent")).toBeNull();
  expect(screen.queryByText("mcp.linear.app")).toBeNull();
  expect(screen.queryByText("docs-mcp --read-only")).toBeNull();
  expect(screen.getByText("Searches the engineering handbook")).toBeDefined();
});

// AGENT ACCESS IS THE ENGINE'S COUNT: each agent's tools on each server it
// reaches, its total and whether it is over the budget, all off `agents`;
// the page adds nothing up. Mutation: flag an agent by comparing its total
// with the budget, and the agent the engine does not flag is named.
test("agent access draws each agent's tools as the engine counts them, and who is over the budget", () => {
  const [backend, pm] = ADMIN_STATUS.agents!;
  current = mcpData("admin", () => {}, {
    agents: [
      { ...backend!, mcp_tools: 104, over_budget: true },
      // Over the figure, and not flagged by the engine: drawn as it answered.
      { ...pm!, mcp_tools: 120, over_budget: false },
    ],
  });
  mountAt({ hash: "#/settings/mcp-tools?view=agents" });
  expect(screen.getByText("Agents by server")).toBeDefined();
  expect(screen.getByText("Backend Engineer has more than 100 MCP tools")).toBeDefined();
  expect(screen.getAllByText("Over 100")).toHaveLength(1);
  expect(screen.getByText("104")).toBeDefined();
  expect(screen.getByText("120")).toBeDefined();
  cleanup();

  current = mcpData("admin", () => {}, { agents: null });
  mountAt({ hash: "#/settings/mcp-tools?view=agents" });
  expect(screen.getByText("Counting each agent's tools")).toBeDefined();
  expect(screen.queryByText("Agents by server")).toBeNull();
});

// EACH AGENT'S TOOLS SAY HOW MANY BUILT-INS THAT AGENT IS OFFERED: the engine
// counts them per agent, since a turn's surfaces leave some off a seat short
// of a grant, so the figure drawn is the agent's own and never one total said
// to be every agent's.
test("an agent's tools say how many built-ins that agent is offered", () => {
  const [backend, pm] = ADMIN_STATUS.agents!;
  current = mcpData("admin", () => {}, {
    agents: [
      { ...backend!, builtin_tools: 31 },
      { ...pm!, builtin_tools: 29 },
    ],
  });
  mountAt({ hash: "#/settings/mcp-tools?view=agents&agent=backend-engineer" });
  expect(
    screen.getByText("Plus 31 built-in tools it is offered, which the budget does not count."),
  ).toBeDefined();
  cleanup();

  mountAt({ hash: "#/settings/mcp-tools?view=agents&agent=pm" });
  expect(
    screen.getByText("Plus 29 built-in tools it is offered, which the budget does not count."),
  ).toBeDefined();
  expect(screen.queryByText(/every agent holds/)).toBeNull();
});

// A CALL WAITING TOO LONG raises its server's alarm, whose fix opens the
// calls waiting on that server, each answerable there.
test("a waiting call's alarm opens the calls held on its server", async () => {
  current = mcpData("admin", () => {}, {
    approvals: [LINEAR_APPROVAL],
    alarms: [
      {
        kind: "mcp_approval_waiting",
        detail: "a call to linear_delete_issue has waited 12 minutes",
        remedy: "",
        entries: [
          {
            key: "linear",
            detail: "a call to linear_delete_issue has waited 12 minutes",
            since: LINEAR_APPROVAL.at,
            seat: "backend-engineer",
            fix: { kind: "approve", server: "linear", seat: "backend-engineer" },
          },
        ],
      },
    ],
  });
  detail = LINEAR_DETAIL;
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(screen.getByText("Backend Engineer is waiting for your approval")).toBeDefined();
  expect(screen.getByText("Waiting on you")).toBeDefined();
  expect(screen.getByText("1 call to approve")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Answer it" }));
  await afterMicrotasks();
  const dialog = screen.getByRole("dialog");
  expect(within(dialog).getByText("A call is waiting")).toBeDefined();
  expect(within(dialog).getByRole("button", { name: "Allow once" })).toBeDefined();
});

// A SERVER'S CHANGED TOOLS raise its alarm, whose fix opens their review, and
// they wait on an administrator beside the held calls.
test("a changed-tools alarm opens their review, and counts as waiting", async () => {
  current = mcpData("admin", () => {}, {
    alarms: [
      {
        kind: "mcp_tools_changed",
        detail: "linear changed delete_issue",
        remedy: "",
        entries: [
          {
            key: "linear",
            detail: "linear changed delete_issue",
            since: null,
            seat: "",
            fix: { kind: "review", server: "linear", seat: "" },
          },
        ],
      },
    ],
  });
  detail = LINEAR_DETAIL;
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(screen.getByText("changed tools on 1 server")).toBeDefined();
  fireEvent.click(screen.getByRole("button", { name: "Review" }));
  await afterMicrotasks();
  expect(within(screen.getByRole("dialog")).getByText("No changes to review")).toBeDefined();
});

// THE SAME SHAPE AS ITS SIBLING SETTINGS PAGES: the views and the one action
// in the page bar, what the page is in its lead note, and no title of its own
// under the bar's breadcrumb.
test("the page's views and Add server sit in the page bar, with no in-page title", () => {
  current = mcpData("admin");
  location.hash = "#/settings/mcp-tools";
  render(
    <TestClient>
      <ViewerProvider>
        <LayerHost>
          <Router>
            <div id="page-lenses" />
            <div id="page-actions" />
            <McpTools />
          </Router>
        </LayerHost>
      </ViewerProvider>
    </TestClient>,
  );
  const bar = document.getElementById("page-actions")!;
  const lenses = document.getElementById("page-lenses")!;
  expect(within(bar).getByRole("button", { name: "Add server" })).toBeDefined();
  expect(within(lenses).getByRole("tablist", { name: "MCP & tools sections" })).toBeDefined();
  expect(screen.queryByRole("heading", { name: "MCP & tools" })).toBeNull();
  expect(screen.getByText(/The MCP servers your agents reach/)).toBeDefined();
});

// AN EMPTY PAGE LEADS WITH ITS EMPTY STATE: no strip of zeros whose lines
// claim every server is enabled and answers.
test("with no server the page draws no figures, and leads with the empty state", () => {
  current = mcpData("admin", () => {}, { servers: [] });
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(document.querySelectorAll(".crewlet-statcard")).toHaveLength(0);
  expect(
    screen.queryByText(/every one enabled|every enabled server answers|every one offered/),
  ).toBeNull();
  expect(screen.getByText("No MCP server yet")).toBeDefined();
});
