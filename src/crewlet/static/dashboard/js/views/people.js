// People directory: every seat in the org chart, agents and humans alike,
// with the identities each one is reachable at.
//
// A pure store read: the org tree arrives on the snapshot, the live agent
// rows and the running-sandbox set come from the projection, so this view
// never fetches anything.

import { esc, escAttr, trunc } from "../format.js";
import { icon } from "../icons.js";
import {
  avatarFor,
  effectiveAgentState,
  integrationMeta,
  roleInk,
  CONTACT_FIELDS,
} from "../state.js";
import { flattenSeats, managerOf } from "../org.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";

export function createPeopleView({ refresh }) {
  // Which kind of seat the directory shows. Local view state, so every
  // store emit re-renders through whichever filter the reader picked.
  let filter = "all";

  // Where a seat is reachable. Each chip is keyed by the contact field it
  // came from — the field set is fixed and ordered by CONTACT_FIELDS, so
  // the key identifies a chip across renders even when a seat gains one.
  function contactChips(seat) {
    const out = [];
    for (const [field, key] of CONTACT_FIELDS) {
      const v = seat.contact && seat.contact[field];
      if (!v) continue;
      const m = integrationMeta(key);
      out.push(
        `<span class="int-badge sm" data-k="c:${escAttr(field)}" style="--int-color:${m.color}" title="${escAttr(field)}">${icon(
          m.icon,
          "sm",
        )}${esc(v)}</span>`,
      );
    }
    if (seat.email) {
      out.push(`<span class="chip" data-k="c:email">${esc(seat.email)}</span>`);
    }
    return out.join("");
  }

  // One directory row, keyed by the seat's own name: that is the identity
  // the engine addresses a seat by (it is what `agents[].role` matches), so
  // the row keeps its node as the filter narrows or the org is edited.
  function seatRow(seat, seats, byRole, sandboxes) {
    const agent = byRole.get(seat.name);
    const human = seat.kind === "human";
    const state = human ? "human" : agent ? effectiveAgentState(agent, sandboxes) : "offline";
    const badge = human
      ? '<span class="badge purple">human</span>'
      : `<span class="badge ${state === "working" ? "live" : state === "idle" ? "done" : state === "afk" ? "afk" : "pending"}"><i class="dot"></i>${esc(state)}</span>`;
    // "agent" navigation is handled by the global delegate in app.js.
    const nav =
      agent && !human ? `class="row clickable" data-action="agent" data-id="${escAttr(agent.id)}"` : 'class="row"';
    const manager = managerOf(seats, seat.name);
    const where = [seat.unitPath.join(" › "), manager ? `reports to ${manager}` : ""]
      .filter(Boolean)
      .join(" · ");

    return `
      <div ${nav} data-k="seat:${escAttr(seat.name)}">
        ${avatarFor(seat.name)}
        <div class="row-body">
          <div class="row-title" style="color:${roleInk(seat.name)}">${esc(seat.name)}
            <span class="org-role-handle">@${esc(seat.handle)}</span></div>
          <div class="row-sub">${esc(where || "root level")}</div>
          ${seat.goal ? `<div class="row-sub">${esc(trunc(seat.goal, 96))}</div>` : ""}
          <div class="chip-row">${contactChips(seat)}</div>
        </div>
        ${seat.availability ? `<span class="badge text">${esc(trunc(seat.availability, 28))}</span>` : ""}
        ${badge}
      </div>`;
  }

  return {
    slices: ["agents", "org", "sandboxes", "health"],

    render(state) {
      const seats = flattenSeats(state.org);
      if (!seats.length) {
        // An org with no seats and one whose `/org` payload has not landed
        // yet flatten to the same empty list, so the connection decides
        // which of the two the reader is shown.
        return state.connected ? empty("users", "No seats configured") : skeletonRows(6);
      }
      // Role name → live agent row, first match wins (matching the `find`
      // this replaced). Hoisted so a large directory doesn't rescan the
      // agent list once per seat on every store emit.
      const byRole = new Map();
      for (const a of state.agents || []) {
        const role = a.role || a.name;
        if (role && !byRole.has(role)) byRole.set(role, a);
      }
      const shown = seats.filter((s) => filter === "all" || s.kind === filter);
      const counts = {
        all: seats.length,
        agent: seats.filter((s) => s.kind === "agent").length,
        human: seats.filter((s) => s.kind === "human").length,
      };
      // `data-k` doubles as the pill's identity for the patcher and as the
      // filter key `onAction` reads back off the clicked element.
      const pill = (key, label) =>
        `<span class="pill ${filter === key ? "active" : ""}" data-action="people-filter" data-k="${key}">${label} <span class="ct">${counts[key]}</span></span>`;

      return `
      ${sectionHead("users", "People directory", seats.length, null)}
      <div class="filters">
        ${pill("all", "Everyone")}${pill("agent", "Agents")}${pill("human", "Humans")}
      </div>
      <div class="list">${shown
        .map((s) => seatRow(s, seats, byRole, state.sandboxes))
        .join("")}</div>`;
    },

    onAction(action, target) {
      if (action !== "people-filter") return;
      filter = target.dataset.k;
      refresh();
    },
  };
}
