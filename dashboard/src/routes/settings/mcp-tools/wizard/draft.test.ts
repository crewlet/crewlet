// @vitest-environment node

import { describe, expect, test } from "vitest";
import type { McpCatalogueEntry, McpValueState } from "~/contract/mcp.ts";
import type { ConfigProblem } from "~/protocol/types.ts";
import {
  emptyDraft,
  entityOf,
  fromCatalogue,
  fromServer,
  nameFromAddress,
  problemAt,
  readPasted,
  stepOf,
  testsValue,
  valuesOf,
  type Draft,
} from "./draft.ts";
import { missingIn } from "./steps.tsx";

/** A curated entry, as `GET /mcp-catalogue` sends one. */
function entry(over: Partial<McpCatalogueEntry> = {}): McpCatalogueEntry {
  return {
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
    docs: "https://linear.app/docs/mcp",
    read_at: "2026-11-01",
    package: "",
    release: "",
    community: false,
    publisher: "",
    version: "",
    updated_at: null,
    ...over,
  };
}

/** Slack's entry: a per-agent local server its integration signs in for, with two fixed switches. */
const slack = entry({
  id: "slack",
  name: "slack",
  title: "Slack",
  vendor: "slack",
  transport: "stdio",
  url: "",
  command: "npm",
  args: ["exec", "--yes", "--", "slack-mcp-server@1.3.0"],
  headers: [],
  env: [
    {
      name: "SLACK_MCP_XOXB_TOKEN",
      description: "",
      required: true,
      secret: true,
      value: "",
      format: "",
      from_integration: true,
    },
    {
      name: "SLACK_MCP_ADD_MESSAGE_TOOL",
      description: "",
      required: false,
      secret: false,
      value: "true",
      format: "",
      from_integration: false,
    },
    {
      name: "SLACK_TEAM",
      description: "",
      required: false,
      secret: false,
      value: "",
      format: "",
      from_integration: false,
    },
  ],
  tool_prefix: "slack_",
  auth: ["integration"],
  integration: "slack",
});

const typed = (d: Draft, key: string, value: string): Draft => ({
  ...d,
  headers: d.headers.map((r) => (r.key === key ? { ...r, value } : r)),
  env: d.env.map((r) => (r.key === key ? { ...r, value } : r)),
});

describe("a catalogue entry", () => {
  // Mutation: keep a key the integration fills as a row, and this is red.
  test("leaves out the keys nobody types, and runs one instance per agent where an integration signs in", () => {
    const d = fromCatalogue(slack, emptyDraft());
    expect(d.signIn).toBe("integration");
    expect(d.shared).toBe(false);
    expect(d.env.map((r) => r.key)).toEqual(["SLACK_TEAM"]);
    const written = entityOf(d, null);
    expect(written).toEqual({
      name: "slack",
      transport: "stdio",
      description: "Issues and projects.",
      tool_prefix: "slack_",
      shared: false,
      command: "npm",
      args: ["exec", "--yes", "--", "slack-mcp-server@1.3.0"],
      env: { SLACK_MCP_ADD_MESSAGE_TOOL: "true" },
    });
  });

  // THE CATALOGUE ORDERS ITS METHODS BY WHAT IT RECOMMENDS, and the draft
  // starts on the first, registering as the client the catalogue names; the
  // header it offers waits as a row in case the person picks it. Mutation:
  // skip OAuth, or ignore the catalogue's client, and this is red.
  test("starts on the method it lists first, as the client it names", () => {
    const d = fromCatalogue(entry({ oauth_client: "dcr" }), emptyDraft());
    expect(d.signIn).toBe("oauth");
    expect(d.oauthClient).toBe("dcr");
    expect(d.headers).toMatchObject([{ key: "Authorization", required: true, stored: false }]);
    expect(entityOf(d, null)).toMatchObject({ oauth: { client: "dcr" } });
    expect(entityOf(d, null)).not.toHaveProperty("headers");
    expect(fromCatalogue(entry({ auth: ["headers", "oauth"] }), emptyDraft()).signIn).toBe(
      "headers",
    );
  });
});

describe("the entry a new server writes", () => {
  // A VALUE NEVER RIDES THE ENTRY. Mutation: write the typed value, or a
  // pointer the page chose, into the headers, and this is red.
  test("leaves a key with a new value out, for the values route to add with its value", () => {
    const d = {
      ...typed(fromCatalogue(entry(), emptyDraft()), "Authorization", "  Bearer lin_not_real  "),
      signIn: "headers" as const,
    };
    const written = entityOf(d, null);
    expect(written).not.toHaveProperty("headers");
    expect(JSON.stringify(written)).not.toContain("lin_not_real");
    expect(valuesOf(d)).toEqual({ Authorization: "Bearer lin_not_real" });
  });

  test("sends no header value where the server is signed in to some other way", () => {
    const d = {
      ...typed(fromCatalogue(entry(), emptyDraft()), "Authorization", "x"),
      signIn: "none" as const,
    };
    expect(valuesOf(d)).toEqual({});
  });
});

describe("an edit", () => {
  const base = {
    name: "linear",
    transport: "http",
    url: "https://mcp.linear.app/mcp",
    enabled: false,
    startup_timeout_seconds: 30,
    tool_annotations: { save_issue: { destructive: false } },
    headers: { Authorization: "${LINEAR_KEY}", "X-Team": "masked", "X-Agent": "${GH}" },
    optional_keys: ["X-Team"],
  };
  const values: McpValueState[] = [
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
    {
      key: "X-Agent",
      as: "header",
      variable: "",
      set: true,
      stored_at: null,
      stored_by: null,
      optional: false,
      bound_by: "github",
    },
  ];

  // AN EDIT WRITES BACK WHAT IT READ. Mutation: build the entry from the
  // draft alone, and the timeout, the annotations and the switch are lost.
  test("writes back every field it does not draw, and each stored key as it is spelled", () => {
    const d = fromServer(base, values, null, "headers");
    expect(d.headers.map((r) => [r.key, r.stored])).toEqual([
      ["Authorization", true],
      ["X-Team", true],
    ]);
    expect(entityOf(d, base)).toEqual(base);
  });

  // Mutation: keep the removed key, or drop the integration's, and this is red.
  test("drops a stored key the person removed, keeping the one an integration fills", () => {
    const d = fromServer(base, values, null, "headers");
    const without = { ...d, headers: d.headers.filter((r) => r.key !== "X-Team") };
    const written = entityOf(without, base);
    expect(written.headers).toEqual({ Authorization: "${LINEAR_KEY}", "X-Agent": "${GH}" });
    expect(written).not.toHaveProperty("optional_keys");
  });

  // Mutation: lay the catalogue's switches over an edit, and this is red.
  test("keeps a catalogue switch as the entry spells it, whatever the catalogue says", () => {
    const saved = {
      name: "slack",
      transport: "stdio",
      command: "npm",
      args: ["exec", "--yes", "--", "slack-mcp-server@1.3.0"],
      shared: false,
      env: { SLACK_MCP_ADD_MESSAGE_TOOL: "false", SLACK_MCP_XOXB_TOKEN: "masked" },
    };
    const bound: McpValueState = { ...values[1]!, key: "SLACK_MCP_XOXB_TOKEN", as: "env" };
    const d = fromServer(saved, [bound], slack, "integration");
    expect(entityOf(d, saved)).toEqual(saved);
  });

  // Mutation: write an OAuth server's headers from its rows, which it has
  // none of, and its other headers and its sign-in are lost.
  test("of a server signed in to with OAuth writes its sign-in and its headers back as they are", () => {
    const saved = {
      name: "notion",
      transport: "http",
      url: "https://mcp.example.com/mcp",
      oauth: { client: "dcr", scopes: ["read"] },
      headers: { "X-Workspace": "${NOTION_WORKSPACE}" },
    };
    const d = fromServer(saved, [], null, "oauth");
    expect(entityOf(d, saved)).toEqual(saved);
    expect(valuesOf(d)).toEqual({});
  });

  test("keeps a stored key as spelled when a new value replaces it", () => {
    const d = typed(fromServer(base, values, null, "headers"), "Authorization", "Bearer new");
    expect(entityOf(d, base).headers).toEqual(base.headers);
    expect(valuesOf(d)).toEqual({ Authorization: "Bearer new" });
  });
});

describe("a pasted configuration", () => {
  // IT IS READ, NEVER RUN, AND ITS VALUES ARE LEFT OUT. Mutation: carry a
  // pasted value into the draft, and this is red.
  test("is read for its servers and their key names, never their values", () => {
    const read = readPasted(
      JSON.stringify({
        mcpServers: {
          "Sentry MCP": {
            url: "https://mcp.sentry.dev/mcp",
            headers: { Authorization: "Bearer sntrys_x" },
          },
          files: { command: "npx", args: ["-y", "files-mcp"], env: { ROOT: "/srv" } },
        },
      }),
    );
    expect(read.state).toBe("read");
    if (read.state !== "read") return;
    expect(read.format).toBe("mcpServers");
    expect(read.servers.map((s) => [s.name, s.transport, s.where, s.keys])).toEqual([
      ["Sentry MCP", "http", "https://mcp.sentry.dev/mcp", ["Authorization"]],
      ["files", "stdio", "npx -y files-mcp", ["ROOT"]],
    ]);
    expect(read.servers[0]!.draft.name).toBe("sentry");
    expect(JSON.stringify(read)).not.toContain("sntrys_x");
    expect(JSON.stringify(read)).not.toContain("/srv");
  });

  test("names VS Code's block and a registry server.json", () => {
    expect(
      readPasted('{"servers":{"a":{"type":"http","url":"https://a.example.com/mcp"}}}'),
    ).toMatchObject({
      state: "read",
      format: "servers",
    });
    expect(
      readPasted(
        '{"name":"io.github.x/notes","remotes":[{"url":"https://notes.example.com/mcp","headers":[{"name":"X-Key"}]}]}',
      ),
    ).toMatchObject({
      state: "read",
      format: "server.json",
      servers: [{ name: "notes", keys: ["X-Key"] }],
    });
  });

  test("says where a block stops being JSON", () => {
    expect(readPasted('{\n  "mcpServers": {\n    "a": \n')).toMatchObject({ state: "error" });
    expect(readPasted("[1]")).toEqual({ state: "error", message: "Paste a JSON object", line: 1 });
    expect(readPasted("{}")).toMatchObject({
      state: "error",
      message: "No mcpServers, servers or remotes",
    });
  });
});

describe("an engine problem", () => {
  const at = (segments: (string | number)[], message = "refused"): ConfigProblem => ({
    path: segments.join("."),
    segments,
    kind: "invalid",
    message,
  });

  test("lands on the step and the field its path names", () => {
    expect(stepOf(at(["mcp_servers", 3, "url"]), "http")).toBe("connection");
    expect(stepOf(at(["mcp_servers", 3, "headers", "Authorization"]), "http")).toBe("signin");
    expect(stepOf(at(["values", "API_KEY"]), "stdio")).toBe("connection");
    expect(stepOf(at(["values", "Authorization"]), "http")).toBe("signin");
    expect(stepOf(at(["roles", 0, "mcp_env"]), "http")).toBe("review");
    const problems = [at(["values", "Authorization"], "the server answered 401")];
    expect(problemAt(problems, "rows", "Authorization")).toBe("the server answered 401");
    expect(problemAt(problems, "rows", "X-Team")).toBeUndefined();
  });
});

test("a name is made from an address's own domain", () => {
  expect(nameFromAddress("https://mcp.linear.app/mcp")).toBe("linear");
  expect(nameFromAddress("https://observability.mcp.cloudflare.com/mcp")).toBe("cloudflare");
  expect(nameFromAddress("not an address")).toBe("");
});

describe("an OAuth sign-in", () => {
  const linear = () => fromCatalogue(entry(), emptyDraft());

  // A PERSON'S SIGN-IN NAMES HOW THE ENGINE REGISTERS: the company's own
  // client with its id, and the scopes. Its secret is a value, stored
  // untested after the entry (a test never renews the grant that sends it),
  // and never written into it. Mutation: write the typed secret, or test it,
  // and this is red.
  test("writes the company's own client and its scopes, and stores its secret untested", () => {
    const d: Draft = {
      ...linear(),
      oauthClient: "own",
      clientId: " crewlet-app ",
      clientSecret: " own-secret-not-real ",
      scopes: "read  write",
    };
    const written = entityOf(d, null);
    expect(written.oauth).toEqual({
      client: "own",
      client_id: "crewlet-app",
      scopes: ["read", "write"],
    });
    expect(JSON.stringify(written)).not.toContain("own-secret-not-real");
    expect(valuesOf(d)).toEqual({ "oauth.client_secret": "own-secret-not-real" });
    expect(testsValue(d, "oauth.client_secret")).toBe(false);
    // A published identity or a registration is a public client: no id, no secret.
    const published = { ...d, oauthClient: "cimd" as const };
    expect(entityOf(published, null).oauth).toEqual({ client: "cimd", scopes: ["read", "write"] });
    expect(valuesOf(published)).toEqual({});
  });

  // A SERVICE IDENTITY NAMES ITS GRANT AND ITS CLIENT'S ID, and leaves its
  // secret for the engine to point where it stores it; the secret is tested
  // by signing in with it. Mutation: name a variable for the secret, or skip
  // its test, and this is red.
  test("writes a service identity's grant and client, and leaves its secret to the engine", () => {
    const d: Draft = {
      ...linear(),
      signIn: "client_credentials",
      clientId: "crewlet-agents",
      clientSecret: "svc-secret-not-real",
      scopes: "read",
    };
    const written = entityOf(d, null);
    expect(written.oauth).toEqual({
      grant: "client_credentials",
      client_id: "crewlet-agents",
      scopes: ["read"],
    });
    expect(written).not.toHaveProperty("headers");
    expect(valuesOf(d)).toEqual({ "oauth.client_secret": "svc-secret-not-real" });
    expect(testsValue(d, "oauth.client_secret")).toBe(true);
  });

  // AN EDIT KEEPS THE SECRET ITS ENTRY READS: the pointer is written back,
  // and nothing is stored again unless a new secret is typed. Mutation: drop
  // the pointer, or ask for the secret again, and this is red.
  test("an edit keeps the secret its entry reads", () => {
    const saved = {
      name: "vendor",
      transport: "http",
      url: "https://mcp.vendor.example.com/mcp",
      oauth: {
        grant: "client_credentials",
        client_id: "crewlet-agents",
        client_secret: "${MCP_VENDOR_OAUTH_CLIENT_SECRET}",
        scopes: ["read"],
      },
    };
    const secret: McpValueState = {
      key: "oauth.client_secret",
      as: "oauth",
      variable: "MCP_VENDOR_OAUTH_CLIENT_SECRET",
      set: true,
      stored_at: null,
      stored_by: null,
      optional: false,
      bound_by: null,
    };
    const d = fromServer(saved, [secret], null, "oauth");
    expect(d.signIn).toBe("client_credentials");
    expect(d.secretStored).toBe(true);
    // NAMED BY THE ENTRY is kept, whatever the store holds for it now.
    expect(fromServer(saved, [{ ...secret, set: false }], null, "oauth").secretStored).toBe(true);
    expect(entityOf(d, saved)).toEqual(saved);
    expect(valuesOf(d)).toEqual({});
    expect(missingIn(d, "signin")).toEqual({});
    expect(missingIn({ ...d, secretStored: false, clientId: "" }, "signin")).toEqual({
      clientId: "Give the id the client was registered under at the vendor.",
      clientSecret: "Give the client's secret: a service identity signs in with it.",
    });
  });
});
