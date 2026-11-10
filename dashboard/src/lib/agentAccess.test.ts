import { expect, test } from "vitest";
import { boxWords, integrationWords, toolServerWords } from "./agentAccess.ts";

// A BOX NO RUN HAS READ SAYS SO rather than claiming what its image lacks.
test("a box is said by where it runs, its memory, and what a run found", () => {
  expect(boxWords({ cell: "e2b" })).toBe("A remote sandbox. No run has read it yet.");
  expect(boxWords({ cell: "container", memory_bytes: 2 ** 31, observed: true })).toBe(
    "A container on the engine host, 2 GB of memory, with no browser and no container runtime.",
  );
  expect(
    boxWords({ cell: "direct", observed: true, browser: "firefox", container_runtime: "docker" }),
  ).toBe(
    "A directory on the engine host, as the engine's user, with a browser (firefox) and a container runtime (docker).",
  );
});

// WHAT AN AGENT MAY WRITE is named; what it may only read is left out.
test("a vendor is said with its level and the permissions it writes with", () => {
  expect(
    integrationWords({
      kind: "github",
      lane: "github",
      on: true,
      permissions: { pull_requests: "write", issues: "read" },
    }),
  ).toBe("github (pull requests: write)");
  expect(
    integrationWords({ kind: "cloudflare", lane: "cloudflare", on: true, level_name: "Operate" }),
  ).toBe("cloudflare (Operate)");
  expect(integrationWords({ kind: "expo", lane: "expo", on: true })).toBe("expo");
});

// A SERVER GRANTED BY ITS DEFAULT GRANTS EVERY TOOL: what the lists do not name
// is said by the default, or by the server's own marks where there is none.
test("a tool server says its named tools and what every other tool does", () => {
  const none = { allow: [], approve: [], blocked: [] };
  expect(toolServerWords({ server: "datadog", ...none, default: "allow" })).toBe(
    "datadog: every tool runs directly",
  );
  expect(toolServerWords({ server: "datadog", ...none })).toBe(
    "datadog: every tool runs at the level its server marks it with",
  );
  expect(
    toolServerWords({
      server: "datadog",
      allow: [],
      approve: ["delete"],
      blocked: [],
      default: "allow",
    }),
  ).toBe("datadog: 1 tool needs a person's approval; every other tool runs directly");
  expect(
    toolServerWords({ server: "linear", allow: ["a", "b"], approve: [], blocked: ["c"], more: 3 }),
  ).toBe(
    "linear: 2 tools run directly, 1 tool blocked, 3 more named tools not shown; every other tool runs at the level its server marks it with",
  );
});

// WHERE AN AGENT ACTS is said: each scope entry by its label, the count past
// the shown ones, and an SSH host with how many read commands it admits.
test("an integration says where it acts", () => {
  expect(
    integrationWords({
      kind: "github",
      lane: "github",
      on: true,
      permissions: { pull_requests: "write" },
      scope: ["r-1", "r-2"],
      scope_labels: { "r-1": "web", "r-2": "api" },
      scope_more: 4,
    }),
  ).toBe("github (pull requests: write) on web, api, 4 more");
  expect(
    integrationWords({
      kind: "ssh",
      lane: "ssh_read",
      on: true,
      scope: ["db-1"],
      commands: [{ host: "db-1", run: ["df -h", "uptime"], more: 1 }],
    }),
  ).toBe("ssh on db-1 (3 read commands)");
});
