// Organization view: hierarchy from /org, cross-referenced with live state.
//
// A pure store read: the org tree arrives on the snapshot and the live agent
// rows come from the projection, so this view never fetches anything.

import { esc, escAttr, trunc } from "../format.js";
import { icon } from "../icons.js";
import {
  effectiveAgentState,
  roleIcon,
  stateBadgeClass,
  stateLabel,
} from "../state.js";
import { empty, skeletonRows } from "../ui.js";

export function createOrgView() {
  // Keys the patcher matches rows on. A node is identified by *where it sits
  // in the tree* — a unit by its full path, a seat by its unit path plus role
  // name — because two units under different parents may share a name, and a
  // role's position in `roles[]` shifts whenever the config is edited.
  const unitKey = (path) => `unit:${path.join("/")}`;
  const roleKey = (path, name) => `role:${path.join("/")}/${name}`;

  function roleRow(r, path, byRole, sandboxes) {
    const agent = byRole.get(r.name);
    let status;
    if (r.kind === "human") {
      status = '<span class="badge purple">human</span>';
    } else if (agent) {
      // Same derivation every other seat surface uses. This row used to
      // hand-roll its own state→badge mapping, so a seat running a
      // detached sandbox showed a grey "pending" chip reading
      // `awaiting_sandbox`, and a failed one showed "pending" too.
      const st = effectiveAgentState(agent, sandboxes);
      status = `<span class="badge ${stateBadgeClass(st)}"><i class="dot"></i>${esc(stateLabel(st))}</span>`;
    } else {
      status = '<span class="badge pending"><i class="dot"></i>offline</span>';
    }
    // "agent" navigation is handled by the global delegate in app.js.
    const clickable = agent ? `data-action="agent" data-id="${escAttr(agent.id)}"` : "";
    return `
      <div class="org-role ${agent ? "clickable" : ""}" data-k="${escAttr(roleKey(path, r.name))}" ${clickable} style="${agent ? "cursor:pointer" : ""}">
        ${icon(roleIcon(r.name))}
        <span class="org-role-name">${esc(r.name)}</span>
        <span class="org-role-handle">@${esc(r.handle)}</span>
        <span class="org-role-goal" style="flex:1">${esc(trunc(r.goal || "", 70))}</span>
        ${status}
      </div>`;
  }

  function unit(u, parentPath, byRole, sandboxes) {
    const path = [...parentPath, u.name];
    const type =
      u.type && u.type !== "team" ? `<span class="unit-type">${esc(u.type)}</span>` : "";
    return `
      <div class="unit" data-k="${escAttr(unitKey(path))}">
        <div class="unit-head">
          ${icon("folder", "sm")}<span>${esc(u.name)}</span>${type}
          ${u.purpose ? `<span class="unit-purpose">${esc(u.purpose)}</span>` : ""}
        </div>
        ${(u.roles || []).map((r) => roleRow(r, path, byRole, sandboxes)).join("")}
        ${(u.children || []).map((c) => unit(c, path, byRole, sandboxes)).join("")}
      </div>`;
  }

  return {
    slices: ["agents", "org", "sandboxes", "health"],

    render(state) {
      const org = state.org || {};
      if (!org.name && !(org.units || []).length && !(org.roles || []).length) {
        // An empty tree means two different things: before the snapshot lands
        // there is simply nothing to say yet, while a connected engine
        // reporting no org is the real answer.
        return state.connected
          ? empty("building", "Organization data unavailable")
          : skeletonRows(6);
      }
      // Role name → live agent row. First match wins, matching the `find`
      // this replaced; the lookup is hoisted so a deep tree doesn't rescan
      // the agent list once per seat.
      const byRole = new Map();
      for (const a of state.agents || []) {
        if (a.role && !byRole.has(a.role)) byRole.set(a.role, a);
      }
      return `
      <div class="org-head">
        <div class="org-name">${esc(org.name || "Organization")}</div>
        ${org.mission ? `<div class="org-mission">${esc(org.mission)}</div>` : ""}
      </div>
      <div class="card" style="padding:8px 14px">
        ${(org.roles || []).map((r) => roleRow(r, [], byRole, state.sandboxes)).join("")}
        ${(org.units || []).map((u) => unit(u, [], byRole, state.sandboxes)).join("") || (!org.roles?.length ? '<div class="empty-sub" style="padding:14px">No units configured</div>' : "")}
      </div>`;
    },
  };
}
