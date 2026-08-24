// Tools view: tools grouped by where they came from.
//
// The server's `source` is the tool-origin grammar in
// crewlet/tools/registry.py: "builtin", "custom" (handed to
// Engine(tools=[...]) by the embedding app), "extension:<name>", or
// "mcp:<server>". Only the last two carry a sub-name.

import { esc, escAttr } from "../format.js";
import { icon } from "../icons.js";
import { emptyOrPending, sectionHead, skeletonRows } from "../ui.js";

function sourceBadge(source) {
  const s = (source || "builtin").toLowerCase();
  if (s === "builtin") return "builtin";
  if (s === "custom") return "custom";
  // Before the substring matches below, not after: an extension called
  // "slack-digest" is not the Slack MCP server, and a badge that says it
  // is puts a third-party tool behind the engine's own colouring.
  if (s.startsWith("extension:")) return "extension";
  if (s.includes("slack")) return "mcp-slack";
  if (s.includes("atlassian") || s.includes("jira") || s.includes("confluence"))
    return "mcp-atlassian";
  if (s.includes("plane")) return "mcp-plane";
  return "mcp";
}

function sourceLabel(source) {
  return (source || "builtin")
    .replace(/^extension:/i, "Extension · ")
    .replace(/^mcp[:_-]?/i, "MCP · ");
}

function groupRank(source) {
  const s = (source || "builtin").toLowerCase();
  if (s === "builtin") return 0;
  if (s === "custom") return 1;
  return s.startsWith("extension:") ? 2 : 3;
}

export function createToolsView({ store }) {
  return {
    slices: ["tools", "health"],

    render(state) {
      const tools = state.tools || [];
      if (!tools.length) {
        return (
          sectionHead("wrench", "Tools", state.connected ? 0 : null) +
          emptyOrPending(
            state,
            () => skeletonRows(5),
            "wrench",
            "No tools registered",
          )
        );
      }
      const groups = {};
      for (const t of tools) {
        const src = t.source || "builtin";
        (groups[src] ||= []).push(t);
      }
      // Ranked, not alphabetical: the groups read outward from the
      // engine — what it ships, what the host app added, what
      // extensions added, what an MCP server serves.
      const order = Object.keys(groups).sort(
        (a, b) => groupRank(a) - groupRank(b) || a.localeCompare(b),
      );

      const sections = order
        .map((src) => {
          const cards = groups[src]
            // A tool name is unique only within its source, so the card key
            // carries the source too — otherwise the same name served by two
            // MCP servers would collide in the patcher's key index.
            .map(
              (t) => `
          <div class="tool-card" data-k="tool:${escAttr(src)}:${escAttr(t.name)}">
            <div class="tool-card-name">${icon("wrench", "sm")} ${esc(t.name)}</div>
            <div class="tool-card-desc">${esc(t.description || "No description")}</div>
            ${
              t.roles && t.roles.length
                ? `<div class="chip-row">${t.roles.map((r) => `<span class="chip">${esc(r)}</span>`).join("")}</div>`
                : ""
            }
          </div>`,
            )
            .join("");
          return `
        <div class="tool-group" data-k="src:${escAttr(src)}">
          <div class="tool-group-head">
            <span class="source-badge ${sourceBadge(src)}">${esc(sourceLabel(src))}</span>
            <span class="sec-count">${groups[src].length}</span>
          </div>
          <div class="tool-grid">${cards}</div>
        </div>`;
        })
        .join("");

      return sectionHead("wrench", "Available Tools", tools.length) + sections;
    },
  };
}
