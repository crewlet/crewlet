// @vitest-environment node

/**
 * The page's words for what the engine decided: a failure the engine could
 * not class is said as such, GitHub's installation refusal is told apart by
 * what the server said, and a refused write says what to do.
 */

import { expect, test } from "vitest";
import { ALARM, FAILURE, failureWords, writeRefusalWords } from "./words.ts";

// A FAILURE WITH NO CLASS is not given one: its words say the engine could not
// tell, and the server's own sentence is what is known.
test("a failure the engine could not class is said as unclassed", () => {
  expect(failureWords("").short).toBe("Failed");
  expect(failureWords("dns")).toBe(FAILURE.dns);
});

// GITHUB'S INSTALLATION REFUSAL IS A MISSING PERMISSION the organization's
// owner approves, told apart by the token route GitHub answered 422 on.
test("GitHub's pending permissions are told apart from any other missing permission", () => {
  expect(
    failureWords("missing_permission", "POST /app/installations/58213907/access_tokens: 422").short,
  ).toBe("Permissions pending");
  expect(failureWords("missing_permission", "403 Forbidden")).toBe(FAILURE.missing_permission);
});

// AN ALARM'S TITLE NAMES WHO A PARTIAL OUTAGE STOPS: the agent, else the node.
test("a partial outage names the agent or the node it stops", () => {
  const about = { server: "Linear", who: "", node: "", count: 0 };
  expect(ALARM.mcp_server_partial.title({ ...about, who: "Product Manager" })).toBe(
    "Linear is not running for Product Manager",
  );
  expect(ALARM.mcp_server_partial.title({ ...about, node: "node-b" })).toBe(
    "Linear is not running on node-b",
  );
  expect(ALARM.mcp_tools_refused.title({ ...about, count: 2 })).toBe(
    "2 Linear tools are not offered",
  );
});

// A WRITE THAT LANDED SAYS NOTHING, and one that may have landed says so.
test("a write's refusal is a sentence, and a write that landed is none", () => {
  expect(
    writeRefusalWords(
      { kind: "saved", revisionId: "r", epoch: 1, warnings: [], derived: null },
      "linear",
      "write",
    ),
  ).toBe("");
  expect(writeRefusalWords({ kind: "missing" }, "linear", "write")).toBe(
    "The configuration no longer has a server called linear.",
  );
  expect(
    writeRefusalWords(
      { kind: "unsettled", answered: false, status: 0, detail: "" },
      "linear",
      "write",
    ),
  ).toContain("may or may not have landed");
});
