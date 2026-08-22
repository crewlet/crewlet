// Configuration: what this company is, how it got that way, and how to
// change it.
//
// The server has shipped a full config write surface for a long time —
// per-entity CRUD for roles, units, LLM providers, MCP servers,
// integrations, budgets and extensions, all versioned, all with
// optimistic concurrency and end-to-end attribution — and the dashboard
// used none of it. It read the active document, and the audit log was a
// second screen that shared sixty copy-pasted lines with this one.
//
// Three lenses now, one room:
//
//   active   the document in force, by section, with the raw payload
//   history  every revision, its diff, and the revert
//   edit     the entity editors, over the CRUD that already exists
//
// The editor is deliberately one generic form over the entity JSON
// rather than eight bespoke ones. Eight forms would be eight partial
// models of a schema that changes, and a form that silently drops a
// field it does not know about is worse than no form: it would delete
// configuration on save. The schema is published (`crewlet schema`), the
// payload round-trips exactly, and the server validates — so the honest
// editor is the one that shows what is there and sends back what you
// wrote.

import { esc, escAttr, prettyJson, fmtDateTime, relTime, shortId } from "../format.js";
import { icon } from "../icons.js";
import { toast } from "../dom.js";
import { apiToken, tokenGate, tokenRejected } from "../authToken.js";
import { confirmModal } from "../modal.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";
import { pushParams } from "../router.js";

// Errors worth retrying on a timer rather than surfacing: the socket is
// down, or the query outlived its answer window.
const RETRIABLE = new Set(["offline", "closed", "timeout"]);
const RETRY_MS = 750;
const RETRY_CEILING_MS = 5_000;

const LENSES = [
  { key: "active", label: "Active" },
  { key: "history", label: "History" },
  { key: "edit", label: "Edit" },
];

// The top-level keys the active document is summarised by, and the icon
// each one wears.
const SECTIONS = [
  ["providers", "cpu", "Providers"],
  ["integrations", "globe", "Integrations"],
  ["mcp_servers", "wrench", "MCP servers"],
  ["units", "building", "Units"],
  ["roles", "users", "Roles"],
  ["extensions", "code", "Extensions"],
  ["scheduling", "clock", "Scheduling"],
];

// The entity collections the API exposes CRUD for. `list` is the query
// that enumerates them; `path` is the REST collection.
const ENTITIES = [
  { key: "roles", label: "Roles", path: "/config/roles", id: "handle" },
  { key: "units", label: "Units", path: "/config/units", id: "name" },
  {
    key: "llm-providers",
    label: "LLM providers",
    path: "/config/llm-providers",
    id: "key",
  },
  {
    key: "mcp-servers",
    label: "MCP servers",
    path: "/config/mcp-servers",
    id: "name",
  },
];

export function createConfigView({ query, refresh, params, askForToken }) {
  let lens = LENSES.some((l) => l.key === params.lens) ? params.lens : "active";

  let doc = null;
  let revisions = null;
  let error = "";
  let unauthorized = false;
  let showRaw = false;
  let disposed = false;
  let retry = RETRY_MS;
  let retryTimer = 0;

  // History state
  const diffs = new Map();
  let openDiff = "";

  // Editor state
  let entityKind = ENTITIES[0].key;
  let entities = null;
  let editing = null; // {id, text, dirty}
  let saving = false;

  function setLens(next) {
    lens = next;
    // A push: the three lenses here are three screens — what is running,
    // what ran before, and an editor — and Back out of the editor should
    // land on the one you opened it from.
    pushParams("config", { lens });
    if (lens === "history" && revisions === null) loadHistory();
    if (lens === "edit" && entities === null) loadEntities();
    refresh();
  }

  // ---- loading ----

  function scheduleRetry(fn) {
    clearTimeout(retryTimer);
    retryTimer = setTimeout(() => {
      if (!disposed) fn();
    }, retry);
    retry = Math.min(retry * 2, RETRY_CEILING_MS);
  }

  async function loadActive() {
    if (!apiToken()) return;
    try {
      doc = await query("config");
      error = "";
      unauthorized = false;
      retry = RETRY_MS;
    } catch (err) {
      const code = err && err.message ? err.message : "failed";
      if (code === "unauthorized") unauthorized = true;
      else if (RETRIABLE.has(code)) scheduleRetry(loadActive);
      else error = code;
    }
    if (!disposed) refresh();
  }

  async function loadHistory() {
    if (!apiToken()) return;
    try {
      const data = await query("config_audit", { limit: 50 });
      revisions = (data && data.revisions) || [];
      error = "";
      retry = RETRY_MS;
    } catch (err) {
      const code = err && err.message ? err.message : "failed";
      if (code === "unauthorized") unauthorized = true;
      else if (RETRIABLE.has(code)) scheduleRetry(loadHistory);
      else error = code;
    }
    if (!disposed) refresh();
  }

  async function loadDiff(revId) {
    if (diffs.has(revId)) return;
    try {
      diffs.set(revId, await query("config_diff", { revision_id: revId }));
    } catch (err) {
      diffs.set(revId, { error: (err && err.message) || "failed" });
    }
    if (!disposed) refresh();
  }

  async function loadEntities() {
    entities = null;
    editing = null;
    try {
      const data = await query("config_entities", { kind: entityKind });
      entities = (data && data.ids) || [];
    } catch (err) {
      const code = (err && err.message) || "failed";
      if (code === "unauthorized") unauthorized = true;
      entities = [];
      error = code;
    }
    if (!disposed) refresh();
  }

  async function openEntity(id) {
    try {
      const data = await query("config_entities", { kind: entityKind, id });
      const text = prettyJson(data.entity);
      // `initial` is what the markup renders and `text` is what the
      // reader has typed. They are separate on purpose: a textarea's
      // content is a child text node, so re-rendering it from the live
      // value on every patch would reset the caret to the top on each
      // keystroke. The markup stays byte-stable while the editor is
      // open, so the patcher computes no change for it at all.
      editing = { id, initial: text, text, dirty: false };
    } catch (err) {
      toast(`Could not read that entity: ${(err && err.message) || "failed"}`, "err");
    }
    if (!disposed) refresh();
  }

  async function saveEntity() {
    if (!editing || saving) return;
    let parsed;
    try {
      parsed = JSON.parse(editing.text);
    } catch (err) {
      // Caught here rather than sent: a malformed body would come back
      // as a 400 with a server-side message about JSON, which is a
      // slower way to learn what the editor already knows.
      toast(`Not valid JSON: ${err.message}`, "err");
      return;
    }
    const spec = ENTITIES.find((e) => e.key === entityKind);
    saving = true;
    refresh();
    let ok = false;
    let detail = "";
    try {
      const res = await fetch(`${spec.path}/${encodeURIComponent(editing.id)}`, {
        method: "PUT",
        headers: {
          Authorization: "Bearer " + apiToken(),
          "Content-Type": "application/json",
          "X-Summary": `edit ${spec.label.toLowerCase()} ${editing.id} via dashboard`,
        },
        body: JSON.stringify(parsed),
      });
      ok = res.ok;
      if (!ok) {
        // 409 is the concurrency answer: somebody else activated a
        // revision while this form was open. Say which one, because the
        // repair is to re-open and re-apply, not to retry blindly.
        const body = await res.json().catch(() => ({}));
        detail =
          res.status === 409
            ? `the config moved on (${body.current_revision_id ? shortId(body.current_revision_id) : "newer revision"}) — reopen and re-apply`
            : body.error || String(res.status);
      }
    } catch {
      detail = "offline";
    }
    saving = false;
    if (!disposed) {
      if (ok) {
        toast("Saved — new revision active", "ok");
        editing = null;
        diffs.clear();
        revisions = null;
        loadActive();
        loadEntities();
      } else {
        toast(`Save failed: ${detail}`, "err");
        refresh();
      }
    }
  }

  async function revert(revId) {
    if (!revId) return;
    const confirmed = await confirmModal({
      title: "Revert to this revision?",
      body: "This does not delete anything. It appends a NEW active revision carrying the old payload, so the history stays intact and this revert is itself revertible.",
      confirm: "Revert",
    });
    if (!confirmed) return;
    let ok = false;
    let code = "";
    // A WRITE, and deliberately still REST. Reads moved to the socket
    // because two transports disagreed and a re-fetch on every streamed
    // round made the page churn; neither applies to a POST fired by an
    // explicit click. It stays on the authenticated route so the
    // revision it creates is attributed to the operator by the same
    // middleware that guards every other write.
    try {
      const res = await fetch(
        `/config/revisions/${encodeURIComponent(revId)}/revert`,
        {
          method: "POST",
          headers: {
            Authorization: "Bearer " + apiToken(),
            "X-Summary": "revert via dashboard",
          },
        },
      );
      ok = res.ok;
      code = String(res.status);
    } catch {
      code = "offline";
    }
    if (disposed) return;
    if (ok) {
      toast("Reverted — new revision active", "ok");
      diffs.clear();
      openDiff = "";
      revisions = null;
      loadHistory();
      loadActive();
    } else {
      toast(`Revert failed (${code})`, "err");
    }
  }

  // ---- render ----

  function lensBar() {
    return `<div class="tok-window">
      ${LENSES.map(
        (l) =>
          `<button class="${l.key === lens ? "active" : ""}" data-action="lens" data-lens="${l.key}">${esc(l.label)}</button>`,
      ).join("")}
    </div>`;
  }

  function countOf(value) {
    if (Array.isArray(value)) return value.length;
    if (value && typeof value === "object") return Object.keys(value).length;
    return 0;
  }

  function activeLens() {
    if (!doc) return skeletonRows(4);
    const payload = doc.payload || doc || {};
    const rows = SECTIONS.map(([key, iconId, label]) => {
      const n = countOf(payload[key]);
      return `<div class="row" data-k="sec:${key}">
        <div class="row-body">
          <div class="row-title">${icon(iconId, "sm")} ${esc(label)}</div>
        </div>
        <span class="badge">${n} ${n === 1 ? "entry" : "entries"}</span>
      </div>`;
    }).join("");
    return `
      <div class="card cfg-active" data-k="cfg:active">
        <div class="eyebrow">Active revision</div>
        <div class="cfg-name display">${esc(payload.name || "unnamed company")}</div>
        <div class="cfg-meta">
          ${doc.revision_id ? `<span class="mono">${esc(shortId(doc.revision_id))}</span>` : ""}
          ${doc.summary ? `<span>${esc(doc.summary)}</span>` : ""}
          ${doc.created_by ? `<span>by ${esc(doc.created_by)}</span>` : ""}
          ${doc.activated_at ? `<span>${esc(relTime(doc.activated_at))}</span>` : ""}
        </div>
      </div>
      <div class="list">${rows}</div>
      <div class="sec">
        <span class="sec-title">Raw payload</span>
        <span class="sec-link" data-action="toggle-raw" role="link" tabindex="0">
          ${showRaw ? "Hide" : "Show"} →
        </span>
      </div>
      ${
        showRaw
          ? `<pre class="code-block">${esc(prettyJson(payload))}</pre>
             <p class="cfg-note">Secrets are redacted by the API before this reaches the browser. A <code>\${VAR}</code> reference is an inert pointer and is shown as written.</p>`
          : ""
      }`;
  }

  function diffPane(revId) {
    const d = diffs.get(revId);
    if (!d) return `<div class="cfg-diff">Reading…</div>`;
    if (d.error) return `<div class="cfg-diff warn-ink">${esc(d.error)}</div>`;
    const changes = d.changes || [];
    if (!changes.length) {
      return `<div class="cfg-diff">No structural difference from the active revision.</div>`;
    }
    return `<div class="cfg-diff">${changes
      .map(
        (c) => `<div class="cfg-change" data-k="ch:${escAttr(c.path)}">
          <span class="cfg-op cfg-op-${esc(c.op)}">${esc(c.op)}</span>
          <span class="mono">${esc(c.path)}</span>
          <span class="cfg-val">${esc(String(c.value ?? "").slice(0, 200))}</span>
        </div>`,
      )
      .join("")}</div>`;
  }

  function historyLens() {
    if (revisions === null) return skeletonRows(5);
    if (!revisions.length) return empty("database", "No revisions recorded");
    return `<div class="list">${revisions
      .map(
        (r) => `
      <div class="cfg-rev" data-k="rev:${escAttr(r.revision_id)}">
        <div class="cfg-rev-top">
          <span class="cfg-dot ${r.is_active ? "on" : ""}"></span>
          <span class="cfg-rev-summary">${esc(r.summary || "no summary")}</span>
          ${r.is_active ? `<span class="badge done">active</span>` : ""}
          <span style="flex:1"></span>
          <span class="btn sm" data-action="diff" data-rev="${escAttr(r.revision_id)}">
            ${openDiff === r.revision_id ? "Hide diff" : "Diff"}
          </span>
          ${
            r.is_active
              ? ""
              : `<span class="btn sm" data-action="revert" data-rev="${escAttr(r.revision_id)}">Revert</span>`
          }
        </div>
        <div class="cfg-rev-meta">
          <span class="mono">${esc(shortId(r.revision_id))}</span>
          <span>${esc(r.created_by || "unknown")}</span>
          <span class="mono">${esc(r.source || "")}</span>
          <span>${esc(fmtDateTime(r.created_at))}</span>
        </div>
        ${openDiff === r.revision_id ? diffPane(r.revision_id) : ""}
      </div>`,
      )
      .join("")}</div>`;
  }

  function editLens() {
    const spec = ENTITIES.find((e) => e.key === entityKind);
    const picker = `<div class="tok-window cfg-kinds">
      ${ENTITIES.map(
        (e) =>
          `<button class="${e.key === entityKind ? "active" : ""}" data-action="kind" data-kind="${e.key}">${esc(e.label)}</button>`,
      ).join("")}
    </div>`;

    if (editing) {
      return `
        ${picker}
        <div class="card cfg-editor" data-k="edit:${escAttr(editing.id)}">
          <div class="sec">
            <span class="sec-title">${esc(spec.label)} · ${esc(editing.id)}</span>
            <span class="sec-link" data-action="close-edit" role="link" tabindex="0">Close →</span>
          </div>
          <textarea class="cfg-text" data-action="edit-text" spellcheck="false"
                    aria-label="${escAttr(spec.label)} ${escAttr(editing.id)} as JSON">${esc(editing.initial)}</textarea>
          <div class="cfg-editor-foot">
            <span class="cfg-note">Saving appends a new revision, attributed to your token. Secrets you cannot see round-trip unchanged.</span>
            <span style="flex:1"></span>
            <button class="btn primary" data-action="save-entity" ${saving ? "disabled" : ""}>
              ${saving ? "Saving…" : "Save"}
            </button>
          </div>
        </div>`;
    }

    if (entities === null) return picker + skeletonRows(4);
    const names = Array.isArray(entities) ? entities : Object.keys(entities || {});
    if (!names.length) {
      return picker + empty("database", `No ${spec.label.toLowerCase()} configured`);
    }
    return (
      picker +
      `<div class="list">${names
        .map((n) => {
          const id = typeof n === "string" ? n : String(n[spec.id] || n.name || "");
          return `<div class="row clickable" data-k="ent:${escAttr(id)}"
                       data-action="open-entity" data-id="${escAttr(id)}">
            <div class="row-body"><div class="row-title">${esc(id)}</div></div>
            <span class="btn sm">Edit</span>
          </div>`;
        })
        .join("")}</div>`
    );
  }

  return {
    slices: ["health"],

    // Back and Forward move between the three lenses without a
    // remount, so what each one needs is loaded here the same way
    // `setLens` loads it — lazily, and once.
    setParams(next = {}) {
      lens = LENSES.some((l) => l.key === next.lens) ? next.lens : "active";
      if (lens === "history" && revisions === null) loadHistory();
      if (lens === "edit" && entities === null) loadEntities();
      refresh();
    },

    mount() {
      loadActive();
      if (lens === "history") loadHistory();
      if (lens === "edit") loadEntities();
    },

    destroy() {
      disposed = true;
      clearTimeout(retryTimer);
    },

    render() {
      if (!apiToken()) return tokenGate("the configuration");
      if (unauthorized) return tokenRejected();
      if (error && lens === "active" && !doc) {
        return empty("database", "Could not read the configuration", esc(error));
      }
      const body =
        lens === "history"
          ? historyLens()
          : lens === "edit"
            ? editLens()
            : activeLens();
      return `${lensBar()}${body}`;
    },

    onAction(action, el) {
      if (action === "lens") setLens(el.dataset.lens);
      else if (action === "toggle-raw") {
        showRaw = !showRaw;
        refresh();
      } else if (action === "diff") {
        const rev = el.dataset.rev;
        openDiff = openDiff === rev ? "" : rev;
        if (openDiff) loadDiff(openDiff);
        refresh();
      } else if (action === "revert") revert(el.dataset.rev);
      else if (action === "kind") {
        entityKind = el.dataset.kind;
        loadEntities();
      } else if (action === "open-entity") openEntity(el.dataset.id);
      else if (action === "close-edit") {
        editing = null;
        refresh();
      } else if (action === "edit-text") {
        // Held in view state, never read back off the DOM: the next
        // patch renders from state, and a value living only in the
        // textarea would be reverted by it.
        if (editing) {
          editing.text = el.value;
          editing.dirty = true;
        }
      } else if (action === "save-entity") saveEntity();
      else if (action === "set-token") {
        // The DIALOG, not a bare setter over stored state. Reading
        // storage and handing the socket back the same missing (or
        // already rejected) value re-renders this very gate, so the button
        // appeared to do nothing — on the one screen whose entire content
        // is behind it.
        askForToken();
      }
    },
  };
}
