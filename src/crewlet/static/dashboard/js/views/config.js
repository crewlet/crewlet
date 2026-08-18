// Configuration view: the active company-config revision, and the config
// itself as the API serves it (secrets already redacted server-side).
//
// Auth-gated. Both `config` and `config_audit` sit behind the operator
// token, which the shell attaches to every query frame — the view never
// passes one itself, it only makes sure the socket has the token the
// reader just typed. Nothing on this screen comes from the store, so
// `slices` is empty and a render only ever follows a load or a click.

import { esc, escAttr, fmtDateTime, prettyJson, shortId } from "../format.js";
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

// Query codes that are about the transport rather than the answer: the
// socket has not opened yet (the shell mounts a view before it connects,
// so a cold load straight onto /config always starts here) or a reply
// went missing. Both are worth asking again; every other code is a real
// answer the reader has to see.
const RETRIABLE = new Set(["offline", "timeout"]);

// Retry cadence for those. Fast enough that the page fills in the moment
// the socket opens, doubling to a ceiling well under the socket's own
// 30s reconnect cap — against a stopped engine each attempt is a locally
// rejected promise, so this costs nothing but a timer.
const RETRY_MS = 750;
const RETRY_MAX_MS = 5_000;

// The one loading placeholder, shared by the first load and by a retry
// that is still waiting on the socket.
const SKELETON = '<div class="skel skel-row" style="margin:24px 0"></div>';

function countOf(value) {
  if (Array.isArray(value)) return value.length;
  if (value && typeof value === "object") return Object.keys(value).length;
  return value == null ? 0 : 1;
}

export function createConfigView({ query, refresh, setToken }) {
  let active = null; // the `config` reply — null means no active revision
  let revisions = []; // `config_audit` revisions, best-effort
  let loaded = false; // the config query has answered at least once
  let loadError = "";
  let raw = false;
  let retryTimer = 0;
  let retryDelay = RETRY_MS;

  async function load() {
    // No token: `render` shows the gate, and asking would only earn an
    // `unauthorized` the reader cannot act on any differently.
    if (!apiToken()) return;
    clearTimeout(retryTimer);
    retryTimer = 0;
    try {
      active = await query("config", {});
    } catch (err) {
      loadError = err.message;
      if (RETRIABLE.has(loadError)) {
        retryTimer = setTimeout(load, retryDelay);
        retryDelay = Math.min(retryDelay * 2, RETRY_MAX_MS);
      }
      refresh();
      return;
    }
    loadError = "";
    loaded = true;
    retryDelay = RETRY_MS;
    refresh();

    // The revision metadata is one line of the header, so it follows the
    // first paint rather than gating it. Losing it (an engine with no
    // config store) costs that line and nothing else on the screen.
    query("config_audit", {})
      .then((audit) => {
        revisions = (audit && audit.revisions) || [];
        refresh();
      })
      .catch(() => {});
  }

  // One summary row per configured top-level key, keyed by that key so
  // the patcher matches rows across renders instead of rewriting them.
  function renderRows() {
    return SECTIONS.filter(([key]) => active[key] !== undefined)
      .map(([key, iconId, blurb]) => {
        const n = countOf(active[key]);
        return `
        <div class="row" data-k="cfg:${escAttr(key)}">
          <span class="row-icon">${icon(iconId, "sm")}</span>
          <div class="row-body">
            <div class="row-title">${esc(key)}</div>
            <div class="row-sub">${esc(blurb)}</div>
          </div>
          <span class="badge">${n} entr${n === 1 ? "y" : "ies"}</span>
        </div>`;
      })
      .join("");
  }

  return {
    // Query-loaded, every last field of it: no push should ever cost
    // this view a render.
    slices: ["health"],

    mount() {
      load();
    },

    render(state) {
      if (!apiToken()) return tokenGate("the company configuration");
      if (loadError === "unauthorized") return tokenRejected();
      if (RETRIABLE.has(loadError)) {
        // A retry is already scheduled, so this is a waiting state and
        // not a dead end. While the socket is still coming up that is
        // indistinguishable from the first load — show the skeleton, and
        // only call it a failure once the engine says it is there.
        return state.connected
          ? empty("database", "Could not load the configuration", "Reconnecting…")
          : SKELETON;
      }
      if (loadError) {
        return `<div class="banner err">${icon("alert", "sm")}<span>Could not load the configuration (${esc(loadError)}).</span></div>`;
      }
      if (!loaded) return SKELETON;

      if (!active) {
        return `
        <div class="banner warn">${icon("alert", "sm")}<span>The engine is unconfigured — import a company config with <code>crewlet config import</code> or <code>PUT /config</code>.</span></div>
        ${empty("database", "No active revision")}`;
      }

      const meta = revisions.find((r) => r.is_active);
      const rows = renderRows();

      return `
      <div class="card" style="padding:16px 18px">
        <div class="eyebrow">Active revision</div>
        <div class="company-name" style="font-size:18px;margin-top:6px">${esc(active.name || "—")}</div>
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
    },

    onAction(action) {
      if (action === "set-token") {
        promptForToken(() => {
          // The bearer rides on the query frame, so the socket has to
          // learn the new token before the reload goes out.
          if (setToken) setToken(apiToken());
          loadError = "";
          loaded = false;
          retryDelay = RETRY_MS;
          load();
          refresh();
        });
      } else if (action === "toggle-raw") {
        // Pure render state: the payload is already in hand, so this
        // costs one re-render and not a round trip.
        raw = !raw;
        refresh();
      }
    },

    destroy() {
      clearTimeout(retryTimer);
    },
  };
}
