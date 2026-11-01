/**
 * The words `lib/mcpServers.ts` puts on the engine's answers: pure, so every
 * rule is pinned without a socket.
 */

import { expect, test } from "vitest";
import {
  NAME_EXAMPLES,
  emptyServerForm,
  formErrors,
  nameExample,
  newPair,
  problemsByField,
  reachOf,
  serverEntity,
  serversUpLine,
  type Pair,
} from "./mcpServers.ts";
import { indexOrg } from "./seats.ts";
import { withDerived } from "~/test/org.ts";

const seatsOf = (roles: unknown[]) => indexOrg(withDerived({ name: "Acme", roles }) as never).seats;

// AN EXAMPLE IS SOMETHING A READER COPIES, so it is never a name the company
// already has — copying one walked straight into "already exists".
test("the name example is never a taken name", () => {
  expect(nameExample(new Set())).toBe(NAME_EXAMPLES[0]);
  expect(nameExample(new Set([NAME_EXAMPLES[0]]))).toBe(NAME_EXAMPLES[1]);
  const all = new Set<string>([...NAME_EXAMPLES, "notes-2"]);
  expect(all.has(nameExample(all))).toBe(false);
});

// A REACH IS THE ENGINE'S GRANT, counted off each agent seat's `tool_sources`.
test("a reach counts the agent seats the engine grants the server", () => {
  const granted = seatsOf([
    { name: "Dev", handle: "dev", tool_sources: ["mcp:github", "mcp:docs"] },
    { name: "PM", handle: "pm", tool_sources: ["mcp:docs"] },
  ]);
  expect(reachOf({ name: "github" }, granted)).toBe("1 seat");
  expect(reachOf({ name: "docs" }, granted)).toBe("Every agent seat");
  expect(reachOf({ name: "linear" }, granted)).toBe("No seat");
});

const up = (started: number, enabled = true) => ({ started, enabled });

// UP MEANS AN INSTANCE STARTED, which `running` and `partial` both are.
test("the servers line counts a server with any instance started", () => {
  expect(serversUpLine([])).toBe("0 of 0 servers running");
  expect(serversUpLine([up(0)])).toBe("0 of 1 server running");
  expect(serversUpLine([up(2), up(1), up(0), up(0)])).toBe("2 of 4 servers running");
});

// A SERVER TURNED OFF IS NOT EXPECTED TO RUN, so it is left out of both counts
// rather than read as one that is down.
test("the servers line leaves out a server that is turned off", () => {
  expect(serversUpLine([up(1), up(0, false)])).toBe("1 of 1 server running");
  expect(serversUpLine([up(0, false)])).toBe("0 of 0 servers running");
});

const row = (key: string, value: string, optional = false): Pair => ({
  ...newPair(),
  key,
  value,
  optional,
});

// A ROW THAT MAY BE UNSET IS AN `optional_keys` ENTRY, by its key, and only
// the chosen transport's rows are sent: a key the other transport's rows hold
// names nothing this entry carries, which the engine refuses.
test("a row that may be unset is sent as the server's optional key", () => {
  const form = {
    ...emptyServerForm(),
    name: "search",
    command: "search-mcp",
    env: [
      row("API_KEY", "${SEARCH_API_KEY}"),
      row("NPM_CONFIG_REGISTRY", "${NPM_CONFIG_REGISTRY}", true),
      row(" PROXY ", "http://${PROXY_HOST}:3128", true),
    ],
    headers: [row("Authorization", "Bearer ${T}", true)],
  };
  expect(serverEntity(form).optional_keys).toEqual(["NPM_CONFIG_REGISTRY", "PROXY"]);
  expect(
    serverEntity({ ...form, transport: "http", url: "mcp.example.com" }).optional_keys,
  ).toEqual(["Authorization"]);
  expect(serverEntity({ ...form, env: [row("API_KEY", "${K}")] })).not.toHaveProperty(
    "optional_keys",
  );
});

// NOTHING IN A LITERAL CAN BE UNSET, so a row may be unset only where its value
// names a secret or a variable — the engine refuses the entry otherwise.
test("a row that may be unset with nothing to resolve is refused before the round trip", () => {
  const form = { ...emptyServerForm(), name: "search", command: "search-mcp" };
  expect(formErrors({ ...form, env: [row("LOG_LEVEL", "debug", true)] }, new Set()).env).toBe(
    "LOG_LEVEL names no secret, so it can’t be unset. Untick its “may be unset” box.",
  );
  expect(
    formErrors({ ...form, env: [row("PROXY", "http://${PROXY_HOST}", true)] }, new Set()).env,
  ).toBeUndefined();
});

// A HEADER IS ONE HEADER IN ANY CASE, as HTTP and the engine read it, so two
// rows spelling one apart are refused before the round trip — while an
// environment tells two variables apart by case, and the form does too.
test("two rows naming one header in two cases are refused, and two variables are not", () => {
  const form = { ...emptyServerForm(), name: "docs", url: "https://docs.example.com/mcp" };
  expect(
    formErrors(
      {
        ...form,
        transport: "http",
        headers: [row("Authorization", "Bearer ${A}"), row("authorization", "Bearer ${B}")],
      },
      new Set(),
    ).headers,
  ).toBe("Authorization and authorization are one header: write it once.");
  expect(
    formErrors(
      { ...form, command: "docs-mcp", env: [row("TOKEN", "${A}"), row("token", "${B}")] },
      new Set(),
    ).env,
  ).toBeUndefined();
});

// THE ENGINE'S REFUSAL OF AN `optional_keys` ENTRY is drawn beside the rows it
// was sent from: the environment's for a command, the headers' for an address.
test("an optional key's problem is placed beside the transport's rows", () => {
  const problem = {
    path: "mcp_servers[1].optional_keys[0]",
    segments: ["mcp_servers", 1, "optional_keys", 0],
    kind: "unknown_value",
    message: "no value written under it carries a reference",
  };
  expect(problemsByField([problem], "stdio").fields).toEqual({ env: problem.message });
  expect(problemsByField([problem], "http").fields).toEqual({ headers: problem.message });
});
