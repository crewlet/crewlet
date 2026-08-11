// Organization view: hierarchy from /org, cross-referenced with live state.

import { esc, trunc } from "../format.js";
import { icon } from "../icons.js";
import { roleIcon } from "../state.js";
import { empty, skeletonRows } from "../ui.js";

export function createOrgView({ store }) {
  let root;

  function agentFor(roleName) {
    return (store.state.agents || []).find((a) => a.role === roleName);
  }

  function roleRow(r) {
    const agent = agentFor(r.name);
    let status;
    if (r.kind === "human") {
      status = '<span class="badge purple">human</span>';
    } else if (agent) {
      const st = agent.state || "offline";
      status = `<span class="badge ${st === "working" ? "live" : st === "idle" ? "done" : st === "afk" ? "afk" : "pending"}"><i class="dot"></i>${esc(st)}</span>`;
    } else {
      status = '<span class="badge pending"><i class="dot"></i>offline</span>';
    }
    const clickable = agent ? `data-action="agent" data-id="${esc(agent.id)}"` : "";
    return `
      <div class="org-role ${agent ? "clickable" : ""}" ${clickable} style="${agent ? "cursor:pointer" : ""}">
        ${icon(roleIcon(r.name))}
        <span class="org-role-name">${esc(r.name)}</span>
        <span class="org-role-handle">@${esc(r.handle)}</span>
        <span class="org-role-goal" style="flex:1">${esc(trunc(r.goal || "", 70))}</span>
        ${status}
      </div>`;
  }

  function unit(u) {
    const type =
      u.type && u.type !== "team" ? `<span class="unit-type">${esc(u.type)}</span>` : "";
    return `
      <div class="unit">
        <div class="unit-head">
          ${icon("folder", "sm")}<span>${esc(u.name)}</span>${type}
          ${u.purpose ? `<span class="unit-purpose">${esc(u.purpose)}</span>` : ""}
        </div>
        ${(u.roles || []).map(roleRow).join("")}
        ${(u.children || []).map(unit).join("")}
      </div>`;
  }

  function render(state) {
    const org = state.org || {};
    if (!org.name && !(org.units || []).length && !(org.roles || []).length) {
      root.innerHTML = empty("building", "Organization data unavailable");
      return;
    }
    root.innerHTML = `
      <div class="org-head">
        <div class="org-name">${esc(org.name || "Organization")}</div>
        ${org.mission ? `<div class="org-mission">${esc(org.mission)}</div>` : ""}
      </div>
      <div class="card" style="padding:8px 14px">
        ${(org.roles || []).map(roleRow).join("")}
        ${(org.units || []).map(unit).join("") || (!org.roles?.length ? '<div class="empty-sub" style="padding:14px">No units configured</div>' : "")}
      </div>`;
  }

  return {
    mount(el) {
      root = el;
      if (!store.state.org || !store.state.org.name) root.innerHTML = skeletonRows(6);
      // "agent" navigation is handled by the global delegate in app.js.
      render(store.state);
    },
    update(state) {
      if (root && root.isConnected) render(state);
    },
  };
}
