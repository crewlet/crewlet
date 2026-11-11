/**
 * The add and edit wizard: what it sends the engine at each step, in what
 * order a save writes, and where the engine's answers land.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import type { McpCatalogueEntry, McpCheckAnswer, McpCheckedTool } from "~/contract/mcp.ts";
import { Router } from "~/app/router.tsx";
import { Store } from "~/protocol/index.ts";
import { TestClient } from "~/test/client.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { createEntity, dryRunCreate, dryRunEntity, putEntity } from "~/protocol/configWrite.ts";
import { readCatalogue, storeValue, testDraft } from "../api.ts";
import type { AccessAnswer } from "~/contract/access.ts";
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

/** Cloudflare, which its integration signs in for each agent it has turned on. */
const CLOUDFLARE: McpCatalogueEntry = {
  ...LINEAR,
  id: "cloudflare",
  name: "cloudflare",
  title: "Cloudflare",
  vendor: "cloudflare",
  description: "The Cloudflare API.",
  url: "https://mcp.cloudflare.com/mcp",
  headers: [{ ...LINEAR.headers[0]!, from_integration: true, format: "Bearer <token>" }],
  auth: ["integration", "oauth", "headers"],
  integration: "cloudflare",
};

/** Cloudflare's Agents tab with the given seats on, the rest off. */
function cloudflareAccess(on: readonly string[]): AccessAnswer {
  const lane = (enabled: boolean) => ({
    desired: { enabled, level: "read" },
    observed: { exists: enabled, enabled },
    status: enabled ? ("in_sync" as const) : ("off" as const),
    plan: [],
    tool: enabled ? ("live" as const) : ("off" as const),
  });
  return {
    kind: "cloudflare",
    revision: "rev-1",
    syncing: false,
    options: [{ lane: "cloudflare", live: false, levels: [{ id: "read", name: "Read" }] }],
    seats: ["backend-engineer", "pm"].map((handle) => ({
      handle,
      name: handle,
      lanes: { cloudflare: lane(on.includes(handle)) },
    })),
  };
}

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

/** Every stub's answer has landed, and what it drew (`test/settle.ts`). */
const settle = afterMicrotasks;

const press = async (name: string) => {
  fireEvent.click(screen.getByRole("button", { name }));
  await settle();
};

/** Sentry from the catalogue, signed in to with a header, its token typed, walked to the review. */
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
  fireEvent.click(screen.getByRole("radio", { name: /^Header/ }));
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
  vi.unstubAllGlobals();
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
  expect(onSaved).toHaveBeenCalledWith("sentry", false);
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
  expect(onSaved).toHaveBeenCalledWith("sentry", false);
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

// AN OAUTH SERVER'S EDIT draws its sign-in as its entry says it (the method
// and the client), and writes the oauth block and its other headers back as
// they are. Mutation: start an OAuth server's edit on another method, or
// rebuild the block from defaults, and this is red.
test("an edit of a server signed in to with OAuth draws its sign-in and writes it back", async () => {
  const entity = {
    name: "linear",
    transport: "http",
    url: "https://mcp.linear.app/mcp",
    oauth: { client: "dcr", scopes: ["read"] },
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
  const checked = (name: RegExp) =>
    (screen.getByRole("radio", { name }) as HTMLInputElement).checked;
  expect(checked(/^OAuth/)).toBe(true);
  expect(checked(/^Register automatically/)).toBe(true);
  expect(screen.getByDisplayValue("read")).toBeDefined();
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
  fireEvent.click(screen.getByRole("radio", { name: /^Header/ }));
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

// A NEW SERVER A PERSON SIGNS IN TO is tested as a draft, which is never sent
// the company's sign-in, so the step says so; it is saved with its oauth
// block alone and the page goes on to its sign-in. Mutation: store a value
// for it, or leave the sign-in for later, and this is red.
test("a new server a person signs in to says its test stops at the sign-in, and signs in next", async () => {
  mount();
  await settle();
  fireEvent.click(screen.getByRole("radio", { name: /Sentry/ }));
  await press("Continue"); // Name
  await press("Continue"); // Connection
  await press("Continue"); // Sign-in: Sentry lists OAuth first
  expect((screen.getByRole("radio", { name: /^OAuth/ }) as HTMLInputElement).checked).toBe(true);
  await press("Continue"); // Who gets it
  await press("Continue"); // Tool approvals, which lists the tools
  await press("Continue"); // Test
  expect(screen.getByText("Tested before anyone signs in")).toBeDefined();
  expect(testDraft).toHaveBeenCalledWith(
    expect.objectContaining({ name: "sentry", oauth: { client: "cimd" } }),
    {},
    "",
  );
  await press("Continue"); // Review
  await press("Add server");
  expect(createEntity).toHaveBeenCalledWith(
    "mcp-servers",
    "sentry",
    expect.objectContaining({ oauth: { client: "cimd" } }),
    "Added the MCP server sentry",
    expect.anything(),
  );
  expect(storeValue).not.toHaveBeenCalled();
  expect(onSaved).toHaveBeenCalledWith("sentry", true);
});

// HOW THE ENGINE REGISTERS IS THE VENDOR'S TO SAY: an authorization server
// that reads no published identity and registers nobody takes the company's
// own client alone, which the step starts on and recommends. Mutation: offer
// a registration the vendor's metadata rules out, and this is red.
test("a vendor that registers nobody is signed in to as the company's own client", async () => {
  vi.mocked(readCatalogue).mockResolvedValueOnce({
    reading: "admin",
    curated: [LINEAR, { ...SENTRY, oauth_client: "own" }],
    community: [],
    registry: "",
    reason: "",
    left_out: 0,
  });
  mount();
  await settle();
  fireEvent.click(screen.getByRole("radio", { name: /Sentry/ }));
  await press("Continue"); // Name
  await press("Continue"); // Connection
  await press("Continue"); // Sign-in
  const client = (name: RegExp) => screen.getByRole("radio", { name }) as HTMLInputElement;
  expect(client(/^Your own client/).checked).toBe(true);
  expect(client(/^Published identity/).disabled).toBe(true);
  expect(client(/^Register automatically/).disabled).toBe(true);
  expect(screen.getByText("Sentry does not read a published identity.")).toBeDefined();
  expect(screen.getByText("https://crewlet.example.com/webhooks/mcp-oauth")).toBeDefined();
});

// A SERVICE IDENTITY'S SECRET IS A VALUE: tested with the draft by signing in
// with it, never written into the entry (the engine points the entry where it
// stores it), then stored write-only and tested. Mutation: write the secret
// into the entry, or skip its test, and this is red.
test("a service identity is tested with its secret, written without it, then its secret stored", async () => {
  mount();
  await settle();
  fireEvent.click(screen.getByRole("radio", { name: /Sentry/ }));
  await press("Continue"); // Name
  await press("Continue"); // Connection
  await press("Continue"); // Sign-in
  fireEvent.click(screen.getByRole("radio", { name: /^Client credentials/ }));
  await press("Continue"); // held: the client's id and secret are missing
  expect(
    screen.getByText("Give the id the client was registered under at the vendor."),
  ).toBeDefined();
  fireEvent.change(screen.getByLabelText("Client ID"), { target: { value: "crewlet-agents" } });
  fireEvent.change(screen.getByLabelText("Client secret"), {
    target: { value: "svc-secret-not-real" },
  });
  await press("Continue"); // Who gets it
  // A service identity is the company's, like any OAuth sign-in.
  expect((screen.getByRole("radio", { name: /^Per agent/ }) as HTMLInputElement).disabled).toBe(
    true,
  );
  await press("Continue"); // Tool approvals, which tests the draft for its tools
  await press("Continue"); // Test
  const draft = { grant: "client_credentials", client_id: "crewlet-agents" };
  expect(testDraft).toHaveBeenCalledWith(
    expect.objectContaining({ oauth: draft }),
    { "oauth.client_secret": "svc-secret-not-real" },
    "",
  );
  await press("Continue"); // Review
  await press("Add server");
  const written = vi.mocked(createEntity).mock.calls[0]![2];
  expect(written.oauth).toEqual(draft);
  expect(JSON.stringify(written)).not.toContain("svc-secret-not-real");
  expect(storeValue).toHaveBeenCalledWith("sentry", "oauth.client_secret", "svc-secret-not-real", {
    test: true,
  });
  expect(onSaved).toHaveBeenCalledWith("sentry", false);
});

/** Cloudflare from the catalogue, on its own sign-in, walked to the Test step. */
async function walkCloudflare() {
  vi.mocked(readCatalogue).mockResolvedValue({
    reading: "admin",
    curated: [LINEAR, SENTRY, CLOUDFLARE],
    community: [],
    registry: "",
    reason: "",
    left_out: 0,
  });
  mount();
  await settle();
  fireEvent.click(screen.getByRole("radio", { name: /Cloudflare/ }));
  await press("Continue"); // Name
  await press("Continue"); // Connection
  await press("Continue"); // Sign-in, the integration's own
  await press("Continue"); // Who gets it, which the integration decides
  await press("Continue"); // Tool approvals
  await press("Continue"); // Test
  await settle();
}

/** Answers the integration's access read, which the Test step lists its agents from. */
function serveAccess(answer: AccessAnswer) {
  const fetched = vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(String(input), "http://engine.test").pathname;
    return path.endsWith("/setup/integrations/cloudflare/access")
      ? new Response(JSON.stringify(answer), { status: 200 })
      : new Response(JSON.stringify({ error: "not_found" }), { status: 404 });
  });
  vi.stubGlobal("fetch", fetched);
  return fetched;
}

// A SERVER AN INTEGRATION SIGNS IN IS TESTED AS AN AGENT IT HAS ON. The test
// defaulted to the chart's first agent, which had Cloudflare off, and failed
// with a refusal that named nothing. Mutation: default to the chart's first
// agent again and the test runs as backend-engineer.
test("a server an integration signs in is tested as an agent the integration has on", async () => {
  const fetched = serveAccess(cloudflareAccess(["pm"]));
  await walkCloudflare();
  expect(fetched.mock.calls.map(([input]) => String(input))).toContainEqual(
    expect.stringContaining("/setup/integrations/cloudflare/access"),
  );
  expect(testDraft).toHaveBeenCalled();
  for (const [, , seat] of vi.mocked(testDraft).mock.calls) expect(seat).toBe("pm");
  expect(screen.getByRole("combobox", { name: "Test as" }).textContent).toBe("Product Manager");
});

// AND NOT AT ALL WHILE IT HAS NONE ON: the test could only fail, so the step
// says where the fix is instead of running it. Mutation: drop the gate and a
// test runs as an agent with Cloudflare off.
test("with no agent on at the integration, the test says to turn one on and runs nothing", async () => {
  serveAccess(cloudflareAccess([]));
  await walkCloudflare();
  expect(testDraft).not.toHaveBeenCalled();
  expect(screen.getByText("No agent has Cloudflare on")).toBeDefined();
  expect(screen.getByRole("link", { name: "Agents tab" }).getAttribute("href")).toBe(
    "#/company/integrations/cloudflare?open=agents",
  );
  expect(screen.queryByRole("combobox", { name: "Test as" })).toBeNull();
});
