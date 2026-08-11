// Tools view: builtin + MCP tools grouped by source.

import { esc } from "../format.js";
import { icon } from "../icons.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";

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
  let root;

  function render(state) {
    const tools = state.tools || [];
    if (!tools.length) {
      root.innerHTML =
        sectionHead("wrench", "Tools", 0) + empty("wrench", "No tools registered");
      return;
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
          .map(
            (t) => `
          <div class="tool-card">
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
        <div class="tool-group">
          <div class="tool-group-head">
            <span class="source-badge ${sourceBadge(src)}">${esc(sourceLabel(src))}</span>
            <span class="sec-count">${groups[src].length}</span>
          </div>
          <div class="tool-grid">${cards}</div>
        </div>`;
      })
      .join("");

    root.innerHTML = sectionHead("wrench", "Available Tools", tools.length) + sections;
  }

  return {
    mount(el) {
      root = el;
      if (!(store.state.tools || []).length) root.innerHTML = skeletonRows(5);
      render(store.state);
    },
    update(state) {
      if (root && root.isConnected) render(state);
    },
  };
}
