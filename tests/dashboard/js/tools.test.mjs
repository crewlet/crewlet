// The Tools view groups by where a tool came from.
//
// Every tool that was not an MCP wrapper used to arrive as source
// "builtin", so a server's tools rendered inside the engine's own group —
// an operator could not tell what the engine ships from what an
// integration added, and a tool missing because its server failed to
// start looked like a missing builtin. The server now sends the real
// origin ("builtin" or "mcp:<server>"); this is the half that has to
// render it as a group of its own.

import assert from "node:assert";
import { installDom } from "./dom.mjs";
import { test, run } from "./harness.mjs";
import { JS_URL } from "./dashboardRoot.mjs";

installDom();
const base = JS_URL;
const { createToolsView } = await import(new URL("views/tools.js", base));

const view = createToolsView({ store: {} });

function tool(name, source) {
  return { name, description: `${name} does a thing`, source, roles: ["Lead"] };
}

function render(tools) {
  return view.render({ tools, connected: true });
}

/** The group headings, in the order they are painted. */
function groupOrder(html) {
  return [...html.matchAll(/class="tool-group" data-k="src:([^"]+)"/g)].map(
    (m) => m[1],
  );
}

/** The badge class + label for one group heading. */
function badge(html, source) {
  const re = new RegExp(
    `data-k="src:${source.replace(/[:.*+?^${}()|[\]\\]/g, "\\$&")}"[\\s\\S]*?` +
      `<span class="source-badge ([^"]+)">([^<]*)</span>`,
  );
  const m = re.exec(html);
  assert.ok(m, `no badge rendered for source ${source}`);
  return { cls: m[1], label: m[2] };
}

test("a server's tools are their own group, not the engine's", () => {
  const html = render([
    tool("lookup_colleague", "builtin"),
    tool("acme_ping", "mcp:acme-metrics"),
  ]);
  assert.deepStrictEqual(groupOrder(html), ["builtin", "mcp:acme-metrics"]);

  const b = badge(html, "mcp:acme-metrics");
  assert.strictEqual(b.cls, "mcp");
  assert.strictEqual(b.label, "MCP · acme-metrics");
});

test("the builtin group still reads as the engine's own", () => {
  const html = render([tool("lookup_colleague", "builtin")]);
  assert.deepStrictEqual(badge(html, "builtin"), {
    cls: "builtin",
    label: "builtin",
  });
});

test("an MCP server's group is unchanged", () => {
  const html = render([tool("jira_get_issue", "mcp:atlassian")]);
  assert.deepStrictEqual(badge(html, "mcp:atlassian"), {
    cls: "mcp-atlassian",
    label: "MCP · atlassian",
  });
});

test("an origin this build cannot produce still renders in its own group", () => {
  // The grammar is "builtin" or "mcp:<server>" and nothing else registers
  // — but this view is served by whatever build the node is running, and a
  // tool the reader can see in a prompt but not in this room is the one
  // failure mode the room exists to prevent. So an unrecognised origin
  // gets a group and the neutral badge, never the builtin group.
  const html = render([
    tool("lookup_colleague", "builtin"),
    tool("acme_ping", "somethingelse:acme"),
  ]);
  assert.deepStrictEqual(groupOrder(html), ["builtin", "somethingelse:acme"]);
  assert.strictEqual(badge(html, "somethingelse:acme").cls, "mcp");
});

test("the builtins come first, then the servers by name", () => {
  // Ranked, not alphabetical: "builtin" happens to sort before "mcp:" but
  // an alphabetical order is an accident of those two strings rather than
  // a rule, and the rule is that the engine's own tools lead.
  const html = render([
    tool("jira_get_issue", "mcp:atlassian"),
    tool("lookup_colleague", "builtin"),
    tool("slack_post", "mcp:slack"),
  ]);
  assert.deepStrictEqual(groupOrder(html), [
    "builtin",
    "mcp:atlassian",
    "mcp:slack",
  ]);
});

test("every card carries a key the patcher can index", () => {
  // Without a data-k the patcher rebuilds the list on every envelope,
  // and a tool name is unique only within its source.
  const html = render([
    tool("ping", "mcp:acme"),
    tool("ping", "mcp:atlassian"),
  ]);
  const keys = [...html.matchAll(/class="tool-card" data-k="([^"]+)"/g)].map(
    (m) => m[1],
  );
  assert.deepStrictEqual(keys, ["tool:mcp:acme:ping", "tool:mcp:atlassian:ping"]);
});

test("a tool with no source at all still lands somewhere", () => {
  // Older engines, and the payload-only fallback before it learned
  // about origins, can both omit it.
  const html = render([{ name: "mystery", description: "" }]);
  assert.deepStrictEqual(groupOrder(html), ["builtin"]);
});

await run();
