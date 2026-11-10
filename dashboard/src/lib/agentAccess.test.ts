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
  expect(
    toolServerWords({ server: "datadog", allow: ["a", "b"], approve: ["c"], blocked: [] }),
  ).toBe("datadog: 2 run directly, 1 with a person's approval");
});
