// Tools view: builtin + MCP tools grouped by source.

import { esc, escAttr } from "../format.js";
import { icon } from "../icons.js";
import { emptyOrPending, sectionHead, skeletonRows } from "../ui.js";

function sourceBadge(source) {
  const s = (source || "builtin").toLowerCase();
  if (s === "builtin") return "builtin";
  if (s.includes("slack")) return "mcp-slack";
  if (s.includes("atlassian") || s.includes("jira") || s.includes("confluence"))
    return "mcp-atlassian";
  if (s.includes("plane")) return "mcp-plane";
  return "mcp";
}

function sourceLabel(source) {
  return (source || "builtin").replace(/^mcp[:_-]?/i, "MCP · ");
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
      const order = Object.keys(groups).sort((a, b) =>
        a === "builtin" ? -1 : b === "builtin" ? 1 : a.localeCompare(b),
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
