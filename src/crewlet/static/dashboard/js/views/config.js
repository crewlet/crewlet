// Configuration view: the active company-config revision, and the config
// itself as the API serves it (secrets already redacted server-side).

import { esc, fmtDateTime, prettyJson, shortId } from "../format.js";
import { icon } from "../icons.js";
import { empty, sectionHead } from "../ui.js";
import { apiToken, promptForToken, tokenGate, tokenRejected } from "../authToken.js";

// Top-level config keys worth their own summary row, in reading order. The
// rest still render in the raw payload below.
const SECTIONS = [
  ["providers", "cpu", "LLM, embeddings, sandbox, queue"],
  ["integrations", "globe", "External surfaces"],
  ["mcp_servers", "wrench", "MCP servers"],
  ["units", "building", "Org units"],
  ["roles", "users", "Root-level seats"],
  ["extensions", "code", "Extensions"],
  ["scheduling", "clock", "Scheduler defaults"],
];

function countOf(value) {
  if (Array.isArray(value)) return value.length;
  if (value && typeof value === "object") return Object.keys(value).length;
  return value == null ? 0 : 1;
}

export function createConfigView({ api }) {
  let root;
  let raw = false;

  async function load() {
    if (!apiToken()) {
      root.innerHTML = tokenGate("the company configuration");
      return;
    }
    root.innerHTML = '<div class="skel skel-row" style="margin:24px 0"></div>';
    const [active, audit] = await Promise.all([
      api.config(apiToken()),
      api.configAudit(apiToken()),
    ]);

    if ((active && active._error === 401) || (audit && audit._error === 401)) {
      root.innerHTML = tokenRejected();
      return;
    }

    if (active && active._error === 404) {
      root.innerHTML = `
        <div class="banner warn">${icon("alert", "sm")}<span>The engine is unconfigured — import a company config with <code>crewlet config import</code> or <code>PUT /config</code>.</span></div>
        ${empty("database", "No active revision")}`;
      return;
    }

    const revisions = (audit && audit.revisions) || [];
    const meta = revisions.find((r) => r.is_active);

    const rows = SECTIONS.filter((s) => active && active[s[0]] !== undefined)
      .map(([key, iconId, blurb]) => {
        const n = countOf(active[key]);
        return `
        <div class="row">
          <span class="row-icon">${icon(iconId, "sm")}</span>
          <div class="row-body">
            <div class="row-title">${esc(key)}</div>
            <div class="row-sub">${esc(blurb)}</div>
          </div>
          <span class="badge">${n} entr${n === 1 ? "y" : "ies"}</span>
        </div>`;
      })
      .join("");

    root.innerHTML = `
      <div class="card" style="padding:16px 18px">
        <div class="eyebrow">Active revision</div>
        <div class="company-name" style="font-size:18px;margin-top:6px">${esc((active && active.name) || "—")}</div>
        <div class="row-sub" style="margin-top:4px">
          <span class="mono">${esc(meta ? shortId(meta.revision_id, 12) : "")}</span>
          ${meta ? `· ${esc(meta.summary || "")} · ${esc(meta.created_by || "")} · ${esc(fmtDateTime(meta.activated_at || meta.created_at))}` : ""}
        </div>
      </div>

      ${sectionHead("database", "Configuration", null, { action: "set-token", label: "change token" })}
      <div class="list">${rows || '<div class="empty-sub" style="padding:14px">Nothing configured</div>'}</div>

      ${sectionHead("code", "Raw payload", null, { action: "toggle-raw", label: raw ? "hide" : "show" })}
      ${
        raw
          ? `<pre class="code">${esc(prettyJson(active))}</pre>`
          : '<div class="empty-sub" style="padding:4px 2px">Secrets are redacted by the API before this reaches the browser.</div>'
      }`;
  }

  return {
    mount(el) {
      root = el;
      load();
    },
    onAction(action) {
      if (action === "set-token") promptForToken(load);
      else if (action === "toggle-raw") {
        raw = !raw;
        load();
      }
    },
  };
}
