// The Tools view groups by where a tool came from.
//
// Every tool that was not an MCP wrapper used to arrive as source
// "builtin", so an extension's tools rendered inside the engine's own
// group — an operator could not tell what the engine ships from what an
// extension added, and a tool missing because its extension failed to
// load looked like a missing builtin. The server now sends the real
// origin ("builtin" / "custom" / "extension:<name>" / "mcp:<server>");
// this is the half that has to render it as a group of its own.

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

test("an extension's tools are their own group, not the engine's", () => {
  const html = render([
    tool("lookup_colleague", "builtin"),
    tool("acme_ping", "extension:acme-metrics"),
  ]);
  assert.deepStrictEqual(groupOrder(html), ["builtin", "extension:acme-metrics"]);

  const b = badge(html, "extension:acme-metrics");
  assert.strictEqual(b.cls, "extension");
  assert.strictEqual(b.label, "Extension · acme-metrics");
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

test("an extension is never badged as the MCP server it is named after", () => {
  // The badge used to be chosen by substring, so an extension called
  // "slack-digest" was painted in the Slack MCP server's colour — a
  // third-party tool wearing an integration's identity.
  const html = render([tool("digest_post", "extension:slack-digest")]);
  assert.strictEqual(badge(html, "extension:slack-digest").cls, "extension");
});

test("tools handed to the engine by the host app are their own group", () => {
  const html = render([tool("review_code", "custom")]);
  assert.deepStrictEqual(badge(html, "custom"), {
    cls: "custom",
    label: "custom",
  });
});

test("groups read outward from the engine", () => {
  const html = render([
    tool("jira_get_issue", "mcp:atlassian"),
    tool("acme_ping", "extension:acme"),
    tool("review_code", "custom"),
    tool("lookup_colleague", "builtin"),
    tool("slack_post", "mcp:slack"),
  ]);
  assert.deepStrictEqual(groupOrder(html), [
    "builtin",
    "custom",
    "extension:acme",
    "mcp:atlassian",
    "mcp:slack",
  ]);
});

test("every card carries a key the patcher can index", () => {
  // Without a data-k the patcher rebuilds the list on every envelope,
  // and a tool name is unique only within its source.
  const html = render([
    tool("ping", "extension:acme"),
    tool("ping", "mcp:atlassian"),
  ]);
  const keys = [...html.matchAll(/class="tool-card" data-k="([^"]+)"/g)].map(
    (m) => m[1],
  );
  assert.deepStrictEqual(keys, ["tool:extension:acme:ping", "tool:mcp:atlassian:ping"]);
});

test("a tool with no source at all still lands somewhere", () => {
  // Older engines, and the payload-only fallback before it learned
  // about extensions, can both omit it.
  const html = render([{ name: "mystery", description: "" }]);
  assert.deepStrictEqual(groupOrder(html), ["builtin"]);
});

await run();
