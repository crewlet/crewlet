/**
 * The add and edit wizard: what it sends the engine at each step, in what
 * order a save writes, and where the engine's answers land.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { McpCatalogueEntry, McpCheckAnswer, McpCheckedTool } from "~/contract/mcp.ts";
import { Router } from "~/app/router.tsx";
import { Store } from "~/protocol/index.ts";
import { TestClient } from "~/test/client.tsx";
import { createEntity, dryRunCreate, dryRunEntity, putEntity } from "~/protocol/configWrite.ts";
import { readCatalogue, storeValue, testDraft } from "../api.ts";
import { MCP_ORG, mcpData } from "../fixtures.ts";
import { ServerWizard, type Saved } from "./ServerWizard.tsx";

const LINEAR: McpCatalogueEntry = {
  id: "linear",
  name: "linear",
  title: "Linear",
  vendor: "linear",
  description: "Issues and projects.",
  transport: "http",
  url: "https://mcp.linear.app/mcp",
  command: "",
  args: [],
  headers: [
    {
      name: "Authorization",
      description: "",
      required: true,
      secret: true,
      value: "",
      format: "Bearer <API key>",
      from_integration: false,
    },
  ],
  env: [],
  tool_prefix: "",
  tool_annotations: {},
  auth: ["oauth", "headers"],
  oauth_client: "cimd",
  integration: "",
  prerequisite: "",
  docs: "",
  read_at: "2026-11-01",
  package: "",
  release: "",
  community: false,
  publisher: "",
  version: "",
  updated_at: null,
};

/** Sentry, which the company does not run yet: what the add tests pick. */
const SENTRY: McpCatalogueEntry = {
  ...LINEAR,
  id: "sentry",
  name: "sentry",
  title: "Sentry",
  vendor: "sentry",
  description: "Errors and issues.",
  url: "https://mcp.sentry.dev/mcp",
};

const PASSED: McpCheckAnswer = {
  server: "linear",
  seat: "",
  draft: true,
  at: "2026-11-01T12:00:00Z",
  by: "jane.doe",
  node: "node-a",
  ok: true,
  nodes: [
    {
      node: "node-a",
      answered: true,
      reason: "",
      ok: true,
      steps: [{ stage: "list", state: "passed", took_ms: 30, detail: "2 tools" }],
      failure: null,
      server_info: { name: "linear-mcp", version: "2.1.0", protocol: "2026-07-28" },
      tools: 2,
      refused: 0,
      took_ms: 120,
    },
  ],
  server_info: { name: "linear-mcp", version: "2.1.0", protocol: "2026-07-28" },
  tools: [],
  warnings: [],
};

const STORED = {
  kind: "ok" as const,
  status: 200,
  value: {
    server: "linear",
    key: "Authorization",
    seat: "",
    variable: "MCP_LINEAR_AUTHORIZATION",
    tested: true,
    check: PASSED,
    pointed: true,
    reload: { revision_id: "r3", epoch: 4 },
    values: [],
  },
};

const VALID = { kind: "valid" as const, baseRevisionId: "r1", warnings: [], derived: null };
const SAVED = { kind: "saved" as const, revisionId: "r2", epoch: 3, warnings: [], derived: null };

vi.mock("../api.ts", async (actual) => ({
  ...(await actual<typeof import("../api.ts")>()),
  readCatalogue: vi.fn(async () => ({
    reading: "admin",
    curated: [LINEAR, SENTRY],
    community: [],
    registry: "",
    reason: "",
    left_out: 0,
  })),
  testDraft: vi.fn(async () => ({ kind: "ok", status: 200, value: PASSED })),
  testServer: vi.fn(async () => ({ kind: "ok", status: 200, value: PASSED })),
  storeValue: vi.fn(),
}));
vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  dryRunCreate: vi.fn(),
  dryRunEntity: vi.fn(),
  createEntity: vi.fn(),
  putEntity: vi.fn(),
  getEntity: vi.fn(async () => ({
    kind: "entity",
    entity: { name: "linear" },
    etag: '"entry-r2"',
  })),
}));

const onSaved = vi.fn();

function mount(
  saved: Saved | null = null,
  start: Parameters<typeof ServerWizard>[0]["start"] = null,
) {
  // THE CHART the grant picker lists, as the store holds it.
  const store = new Store();
  store.applyOrg(MCP_ORG as never);
  render(
    <TestClient store={store}>
      <ToastProvider>
        <LayerHost>
          <Router>
            <ServerWizard
              data={mcpData("admin")}
              saved={saved}
              start={start}
              onClose={() => {}}
              onSaved={onSaved}
            />
          </Router>
        </LayerHost>
      </ToastProvider>
    </TestClient>,
  );
}

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

const press = async (name: string) => {
  fireEvent.click(screen.getByRole("button", { name }));
  await settle();
};

/** Sentry from the catalogue, its token typed, walked to the review. */
async function walkSentry() {
  mount();
  await settle();
  // WHAT THE COMPANY RUNS stands apart as added, never offered twice.
  expect(screen.queryByRole("radio", { name: /Linear/ })).toBeNull();
  expect(screen.getByRole("link", { name: "Open Linear" })).toBeDefined();
  fireEvent.click(screen.getByRole("radio", { name: /Sentry/ }));
  await press("Continue"); // Name
  await press("Continue"); // Connection
  await press("Continue"); // Sign-in
  fireEvent.change(screen.getByLabelText("Value for Authorization"), {
    target: { value: "  Bearer sntrys_not_real " },
  });
  await press("Continue"); // Who gets it
  await press("Continue"); // Tool approvals, which lists the tools
  await press("Continue"); // Test
  await press("Continue"); // Review
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(dryRunCreate).mockResolvedValue(VALID);
  vi.mocked(dryRunEntity).mockResolvedValue(VALID);
  vi.mocked(createEntity).mockResolvedValue(SAVED);
  vi.mocked(putEntity).mockResolvedValue(SAVED);
  vi.mocked(storeValue).mockResolvedValue(STORED);
});

afterEach(() => {
  cleanup();
  location.hash = "";
});

// THE TEST IS THE DRAFT WITH ITS TYPED VALUES, AND THE SAVE IS ENTRY THEN
// VALUE: the entry is written with no value in it, and the value is stored
// write-only, tested, through its own route. Mutation: carry the value in
// the entry, or skip the value's test, and this is red.
test("a server added from the catalogue is tested with its value, written, then its value stored", async () => {
  await walkSentry();
  expect(readCatalogue).toHaveBeenCalledWith("", expect.anything());
  expect(testDraft).toHaveBeenCalledWith(
    expect.objectContaining({ name: "sentry", url: "https://mcp.sentry.dev/mcp" }),
    { Authorization: "Bearer sntrys_not_real" },
    "",
  );
  const tested = vi.mocked(testDraft).mock.calls[0]![0];
  expect(tested).not.toHaveProperty("headers");
  expect(screen.getByText("The engine accepts this server")).toBeDefined();
  await press("Add server");
  expect(createEntity).toHaveBeenCalledWith(
    "mcp-servers",
    "sentry",
    {
      name: "sentry",
      transport: "http",
      description: "Errors and issues.",
      url: "https://mcp.sentry.dev/mcp",
    },
    "Added the MCP server sentry",
    expect.anything(),
  );
  expect(storeValue).toHaveBeenCalledWith("sentry", "Authorization", "Bearer sntrys_not_real", {
    test: true,
  });
  expect(onSaved).toHaveBeenCalledWith("sentry");
});

// A VALUE THE SERVER REFUSES leaves the entry saved and the wizard open on
// what is left, and a second Save sends only that. Mutation: write the
// entry again on the retry, and this is red.
test("a value refused at save leaves the entry saved, and the retry sends only the value", async () => {
  vi.mocked(storeValue).mockResolvedValueOnce({
    kind: "refused",
    status: 422,
    code: "value_test_failed",
    sentence: "MCP server sentry did not work with this value on node-a. Nothing was stored.",
    body: {
      check: { ...PASSED, ok: false },
      problems: [
        {
          path: "values.Authorization",
          segments: ["values", "Authorization"],
          kind: "invalid",
          message: "The server answered 401.",
        },
      ],
    },
  });
  await walkSentry();
  await press("Add server");
  expect(createEntity).toHaveBeenCalledTimes(1);
  expect(onSaved).not.toHaveBeenCalled();
  expect(screen.getByText("Saved, with a value still to store")).toBeDefined();
  await press("Add server");
  expect(createEntity).toHaveBeenCalledTimes(1);
  expect(putEntity).not.toHaveBeenCalled();
  expect(storeValue).toHaveBeenCalledTimes(2);
  expect(onSaved).toHaveBeenCalledWith("sentry");
});

// THE ENGINE DECIDES: its dry run's problem lands on the field it names and
// marks that step, and nothing is saved. Mutation: drop the problem's
// placement, and this is red.
test("an engine problem lands on its field and holds the save", async () => {
  vi.mocked(dryRunCreate).mockResolvedValue({
    kind: "problems",
    problems: [
      {
        path: "mcp_servers[0].url",
        segments: ["mcp_servers", 0, "url"],
        kind: "invalid",
        message: "The address is plain http to a public host: use https.",
      },
    ],
    derived: null,
    code: "",
    hint: "",
  });
  await walkSentry();
  expect(screen.getByText("The engine refused this")).toBeDefined();
  const add = screen.getByRole("button", { name: "Add server" });
  expect(add.hasAttribute("disabled") || add.getAttribute("aria-disabled") === "true").toBe(true);
  fireEvent.click(screen.getByRole("button", { name: /Connection/ }));
  await settle();
  expect(screen.getByText("The address is plain http to a public host: use https.")).toBeDefined();
  expect(createEntity).not.toHaveBeenCalled();
});

// AN EDIT KEEPS ITS NAME and writes the entry back against the revision it
// lands on, with nothing it does not draw dropped. Mutation: create on an
// edit, or rebuild the entry from the draft alone, and this is red.
test("an edit writes the entry back, every field it does not draw kept", async () => {
  const entity = {
    name: "linear",
    transport: "http",
    url: "https://mcp.linear.app/mcp",
    startup_timeout_seconds: 30,
    headers: { Authorization: "${LINEAR_KEY}" },
  };
  mount(
    {
      server: mcpData("admin").server("linear")!,
      entity,
      values: [
        {
          key: "Authorization",
          as: "header",
          variable: "LINEAR_KEY",
          set: true,
          stored_at: null,
          stored_by: null,
          optional: false,
          bound_by: null,
        },
      ],
      entry: LINEAR,
      signIn: "headers",
    },
    "connection",
  );
  await settle();
  expect(screen.queryByRole("button", { name: /Source/ })).toBeNull();
  fireEvent.change(screen.getByLabelText("Tool prefix"), { target: { value: "lin_" } });
  await press("Review changes");
  expect(dryRunEntity).toHaveBeenCalledWith(
    "mcp-servers",
    "linear",
    { ...entity, tool_prefix: "lin_" },
    '"entry-r2"',
    expect.anything(),
  );
  await press("Save changes");
  expect(putEntity).toHaveBeenCalledWith(
    "mcp-servers",
    "linear",
    { ...entity, tool_prefix: "lin_" },
    '"entry-r2"',
    "Edited the MCP server linear",
    expect.anything(),
  );
  expect(createEntity).not.toHaveBeenCalled();
  expect(storeValue).not.toHaveBeenCalled();
});

// AN OAUTH SERVER'S EDIT keeps its sign-in: the wizard does not set one up,
// so it offers no choice that would replace it, and writes the `oauth` block
// and the other headers back untouched. Mutation: draw the choices, or write
// the headers from rows it has none of, and this is red.
test("an edit of a server signed in to with OAuth keeps its sign-in as it is", async () => {
  const entity = {
    name: "linear",
    transport: "http",
    url: "https://mcp.linear.app/mcp",
    oauth: { scopes: ["read"] },
    headers: { "X-Team": "${LINEAR_TEAM}" },
  };
  mount(
    {
      server: mcpData("admin").server("linear")!,
      entity,
      values: [],
      entry: LINEAR,
      signIn: "oauth",
    },
    "signin",
  );
  await settle();
  expect(screen.getByText("Linear signs in with OAuth")).toBeDefined();
  expect(screen.queryByRole("radiogroup", { name: "Sign-in" })).toBeNull();
  await press("Review changes");
  expect(dryRunEntity).toHaveBeenCalledWith(
    "mcp-servers",
    "linear",
    entity,
    '"entry-r2"',
    expect.anything(),
  );
  expect(storeValue).not.toHaveBeenCalled();
});

/** A tool a test listed, at the level the engine starts it at. */
const listedTool = (raw: string, over: Partial<McpCheckedTool> = {}): McpCheckedTool => ({
  name: raw,
  raw,
  title: "",
  description: "",
  annotations: {
    read_only: "unknown",
    destructive: "unknown",
    idempotent: "unknown",
    open_world: "unknown",
  },
  risk: "destructive",
  risk_declared: false,
  external: true,
  starting_level: "approve",
  refused: "",
  ...over,
});

// WHO GETS IT AND EACH TOOL'S LEVEL are written on the entry: the approvals
// step is handed every tool the draft's test listed (the test sent without
// the draft's levels), each at the engine's starting level, and only a level
// changed from it is written. Mutation: send the levels with the test, or
// write every tool's starting level, and this is red.
test("a server is added with the agents chosen for it and the levels set on its tools", async () => {
  vi.mocked(testDraft).mockResolvedValue({
    kind: "ok",
    status: 200,
    value: {
      ...PASSED,
      tools: [
        listedTool("find_issue", {
          risk: "read_only",
          risk_declared: true,
          starting_level: "allow",
        }),
        listedTool("drop_issue"),
      ],
    },
  });
  mount();
  await settle();
  fireEvent.click(screen.getByRole("radio", { name: /Sentry/ }));
  await press("Continue"); // Name
  await press("Continue"); // Connection
  await press("Continue"); // Sign-in
  fireEvent.change(screen.getByLabelText("Value for Authorization"), {
    target: { value: "Bearer sntrys_not_real" },
  });
  await press("Continue"); // Who gets it
  fireEvent.click(screen.getByRole("radio", { name: /Chosen teams and agents/ }));
  fireEvent.click(screen.getByRole("checkbox", { name: "Product Manager" }));
  await press("Continue"); // Tool approvals: the test lists the tools
  expect(testDraft).toHaveBeenCalledTimes(1);
  expect(vi.mocked(testDraft).mock.calls[0]![0]).not.toHaveProperty("tools");
  expect(screen.getByRole("combobox", { name: "Level for find_issue" }).textContent).toContain(
    "Always allow",
  );
  fireEvent.click(screen.getByRole("combobox", { name: "Level for drop_issue" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "Blocked" }));
  await settle();
  // A level set back to where the engine starts the tool is no level of its own.
  fireEvent.click(screen.getByRole("combobox", { name: "Level for find_issue" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "Needs approval" }));
  await settle();
  fireEvent.click(screen.getByRole("combobox", { name: "Level for find_issue" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "Always allow" }));
  await settle();
  await press("Continue"); // Test
  // A test asked again, with a level set, is still sent without the levels.
  await press("Test again");
  expect(testDraft).toHaveBeenCalledTimes(2);
  expect(vi.mocked(testDraft).mock.calls[1]![0]).not.toHaveProperty("tools");
  await press("Continue"); // Review
  expect(screen.getByText("1 agent")).toBeDefined();
  expect(screen.getByText("1 always allow and 1 blocked")).toBeDefined();
  await press("Add server");
  expect(createEntity).toHaveBeenCalledWith(
    "mcp-servers",
    "sentry",
    expect.objectContaining({
      access: { seats: ["pm"] },
      tools: { levels: { drop_issue: "blocked" } },
    }),
    "Added the MCP server sentry",
    expect.anything(),
  );
});
