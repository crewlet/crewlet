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
import { LINEAR_DETAIL, LINEAR_MEMBER_DETAIL, MCP_NOW, mcpData } from "./fixtures.ts";
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
  { hash: "#/settings/mcp-tools?view=builtin" },
  { hash: "#/settings/mcp-tools?flow=add" },
  { hash: "#/settings/mcp-tools?flow=edit&server=linear&step=connection" },
  { hash: "#/settings/mcp-tools?flow=test&server=linear" },
  { hash: "#/settings/mcp-tools?flow=value&server=linear&key=Authorization" },
  { hash: "#/settings/mcp-tools?flow=restart&server=linear" },
  { hash: "#/settings/mcp-tools?flow=disable&server=linear" },
  { hash: "#/settings/mcp-tools?flow=remove&server=linear" },
  { hash: "#/settings/mcp-tools/servers/linear", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?tab=connection", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?tab=tools", server: "linear" },
  { hash: "#/settings/mcp-tools/servers/linear?tab=activity", server: "linear" },
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
  for (const name of [/^Add server$/, /^Restart$/, /^Actions for /, /^Edit/, /^Test now$/]) {
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
  current = mcpData("admin");
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

// THE LIST: the header names the page and its one action, the tabs its two
// views, and a member reads that the page is theirs to read only.
test("the list names the page, its views and, for a member, why it is read only", () => {
  current = mcpData("admin");
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(screen.getByRole("heading", { name: "MCP & tools" })).toBeDefined();
  const tabs = screen.getByRole("tablist", { name: "MCP & tools sections" });
  expect(within(tabs).getByRole("tab", { name: /Servers/ })).toBeDefined();
  expect(within(tabs).getByRole("tab", { name: /Built-in tools/ })).toBeDefined();
  expect(screen.queryByText("Read-only. Changes take Admin access.")).toBeNull();
  // FOUR FIGURES for an administrator, the fourth what the alarms ask of them.
  expect(document.querySelectorAll(".crewlet-statcard")).toHaveLength(4);
  expect(screen.getAllByText("Needs attention").length).toBeGreaterThan(1);
  cleanup();

  current = mcpData("member");
  mountAt({ hash: "#/settings/mcp-tools" });
  expect(screen.getByText("Read-only. Changes take Admin access.")).toBeDefined();
  expect(document.querySelectorAll(".crewlet-statcard")).toHaveLength(3);
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
