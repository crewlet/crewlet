// Audit log: the company config's revision history — who changed what,
// when, the structural diff, and revert.

import { esc, fmtDateTime, prettyJson, shortId, trunc } from "../format.js";
import { icon } from "../icons.js";
import { empty, sectionHead } from "../ui.js";
import { toast } from "../dom.js";
import { apiToken, promptForToken, tokenGate, tokenRejected } from "../authToken.js";

export function createAuditView({ api }) {
  let root;

  async function load() {
    if (!apiToken()) {
      root.innerHTML = tokenGate("the audit log");
      return;
    }
    root.innerHTML = '<div class="skel skel-row" style="margin:24px 0"></div>';
    const audit = await api.configAudit(apiToken());
    if (audit && audit._error === 401) {
      root.innerHTML = tokenRejected();
      return;
    }
    const revisions = (audit && audit.revisions) || [];

    const history = revisions
      .map(
        (r) => `
      <div class="rev">
        <span class="rev-mark ${r.is_active ? "active" : ""}">${r.is_active ? "●" : "○"}</span>
        <div class="row-body">
          <div class="row-title" style="font-weight:550">${esc(r.summary || "(no summary)")}</div>
          <div class="row-sub"><span class="mono">${esc(shortId(r.revision_id, 12))}</span> · ${esc(r.created_by || "")} · ${esc(r.source || "")} · ${esc(fmtDateTime(r.activated_at || r.created_at))}</div>
        </div>
        <button class="btn sm" data-action="diff" data-id="${esc(r.revision_id)}">${icon("git", "sm")} Diff</button>
        ${!r.is_active ? `<button class="btn sm" data-action="revert" data-id="${esc(r.revision_id)}">Revert</button>` : ""}
      </div>`,
      )
      .join("");

    root.innerHTML = `
      ${sectionHead("clipboard", "Revision history", revisions.length, { action: "set-token", label: "change token" })}
      ${revisions.length ? `<div class="list">${history}</div>` : empty("clipboard", "No revisions yet")}
      <div id="diff-pane" style="margin-top:16px"></div>`;
  }

  async function showDiff(revId) {
    const pane = root.querySelector("#diff-pane");
    if (!pane) return;
    pane.innerHTML = '<div class="skel skel-row"></div>';
    const d = await api.configDiff(revId, apiToken());
    if (!d || d._error) {
      pane.innerHTML = `<div class="banner err">${icon("alert", "sm")}<span>Could not load diff.</span></div>`;
      return;
    }
    const changes = (d.changes || [])
      .map((c) => {
        const sym = c.op === "add" ? "+" : c.op === "remove" ? "−" : "~";
        const val =
          c.value !== undefined ? trunc(prettyJson(c.value).replace(/\s+/g, " "), 200) : "";
        return `<div class="row" style="padding:7px 14px">
          <span class="diff-op ${esc(c.op)}">${sym}</span>
          <span class="diff-path">${esc(c.path)}</span>
          <span class="row-sub" style="margin:0;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(val)}</span>
        </div>`;
      })
      .join("");
    pane.innerHTML = `
      <div class="card">
        <div class="sec" style="margin:12px 16px"><span class="sec-title">Diff
          <span class="sec-count">${esc(shortId(d.from || "", 8))} → ${esc(shortId(d.to || "", 8))}</span></span></div>
        ${changes || '<div class="empty-sub" style="padding:14px">No structural changes</div>'}
      </div>`;
  }

  async function revert(revId) {
    if (!window.confirm("Create a new active revision from this one?")) return;
    const r = await api.revertConfig(revId, "revert via dashboard", apiToken());
    if (r && !r._error) {
      toast("Reverted — new revision active", "ok");
      load();
    } else {
      toast("Revert failed" + (r && r._error ? ` (${r._error})` : ""), "err");
    }
  }

  return {
    mount(el) {
      root = el;
      load();
    },
    onAction(action, t) {
      if (action === "set-token") promptForToken(load);
      else if (action === "diff") showDiff(t.dataset.id);
      else if (action === "revert") revert(t.dataset.id);
    },
  };
}
